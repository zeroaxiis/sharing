package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// ErrNilChannel is returned when Browse is handed no channel to emit on.
var ErrNilChannel = errors.New("discovery: browse needs a non-nil event channel")

// Browse watches the local network for peers and emits add, update and remove
// events on out until ctx is cancelled. It closes out before returning, so a
// consumer can simply range over the channel.
//
// A cancelled context is a clean shutdown and returns nil. Transient failures -
// no multicast interface, a firewall that ate the socket, an adapter that came
// and went - are logged and retried with backoff rather than returned, because
// on a laptop they routinely fix themselves and a daemon that gave up on
// discovery at boot would stay blind for the rest of the session.
func Browse(ctx context.Context, logger *slog.Logger, selfID string, out chan<- Event) error {
	if logger == nil {
		logger = slog.Default()
	}
	if out == nil {
		return fmt.Errorf("discovery: browse: %w", ErrNilChannel)
	}
	defer close(out)

	logFirewallNotice(logger)
	logger.Info("mDNS browse started",
		"serviceType", ServiceType,
		"domain", Domain,
		"selfId", selfID,
		"deviceTtl", DeviceTTL,
		"cycle", browseCycle,
	)

	b := &browser{
		logger:       logger,
		selfID:       selfID,
		known:        make(map[string]*sighting),
		pref:         localAddrPref(),
		prefAt:       time.Now(),
		refreshEvery: browseCycle,
	}

	// The zeroconf browse runs in its own goroutine and feeds raw sightings in
	// here, so the liveness sweep keeps ticking even while a cycle is between
	// restarts.
	raw := make(chan *zeroconf.ServiceEntry, 64)
	cycleCtx, stopCycles := context.WithCancel(ctx)
	var cycles sync.WaitGroup
	cycles.Add(1)
	go func() {
		defer cycles.Done()
		defer close(raw)
		b.runCycles(cycleCtx, raw)
	}()
	defer func() {
		stopCycles()
		// Keep reading so the cycle goroutine can never wedge on a full raw
		// channel while it winds down.
		go func() {
			for range raw {
			}
		}()
		cycles.Wait()
	}()

	ticker := time.NewTicker(livenessTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Debug("mDNS browse stopping", "known", len(b.known))
			return nil

		case entry, ok := <-raw:
			if !ok {
				return nil
			}
			ev, emit := b.observe(entry, time.Now())
			if emit && !b.send(ctx, out, ev) {
				return nil
			}

		case t := <-ticker.C:
			for _, ev := range b.sweep(t) {
				if !b.send(ctx, out, ev) {
					return nil
				}
			}
		}
	}
}

// send delivers one event, giving up if the consumer has stopped reading and the
// context is done. It reports whether browsing should continue.
func (b *browser) send(ctx context.Context, out chan<- Event, ev Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// -----------------------------------------------------------------------------
// browse cycles
// -----------------------------------------------------------------------------

// runCycles restarts the underlying zeroconf browse forever. See browseCycle for
// why the restart is required rather than merely tidy.
func (b *browser) runCycles(ctx context.Context, out chan<- *zeroconf.ServiceEntry) {
	backoff := retryBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := b.oneCycle(ctx, out)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.logger.Warn("mDNS browse cycle failed, will retry",
				"error", err, "retryIn", backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, retryBackoffMax)
			continue
		}
		backoff = retryBackoffMin
		// A cycle that returns immediately (a socket torn down under us, say)
		// must not turn into a spin loop.
		if elapsed := time.Since(started); elapsed < time.Second {
			if !sleepCtx(ctx, time.Second-elapsed) {
				return
			}
		}
	}
}

// oneCycle runs a single zeroconf browse for browseCycle and forwards every
// entry it produces.
func (b *browser) oneCycle(ctx context.Context, out chan<- *zeroconf.ServiceEntry) error {
	cycleCtx, cancel := context.WithTimeout(ctx, browseCycle)
	defer cancel()

	entries := make(chan *zeroconf.ServiceEntry, 32)
	errc := make(chan error, 1)
	go func() {
		errc <- zeroconf.Browse(cycleCtx, ServiceType, Domain, entries)
	}()

	var (
		browseDone bool
		entriesEnd bool
	)
	for {
		if browseDone && entriesEnd {
			return nil
		}
		select {
		case entry, ok := <-entries:
			if !ok {
				entries = nil
				entriesEnd = true
				continue
			}
			select {
			case out <- entry:
			case <-ctx.Done():
				go drainEntries(entries, errc)
				return nil
			}

		case err := <-errc:
			errc = nil
			browseDone = true
			if err != nil {
				// zeroconf only fails this early when it could not bind or join
				// the multicast group, in which case it never started the reader
				// that closes entries. Nothing more is coming.
				return fmt.Errorf("discovery: browse %s in %s: %w", ServiceType, Domain, err)
			}

		case <-ctx.Done():
			go drainEntries(entries, errc)
			return nil
		}
	}
}

// drainEntries empties a zeroconf entry channel that nobody is reading any more,
// so the library goroutine writing into it can finish and exit.
func drainEntries(entries <-chan *zeroconf.ServiceEntry, errc <-chan error) {
	for {
		select {
		case _, ok := <-entries:
			if !ok {
				return
			}
		case <-errc:
			// zeroconf writes here only after its reader goroutine has returned,
			// so there is nothing left to drain.
			return
		}
	}
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// -----------------------------------------------------------------------------
// liveness tracking
// -----------------------------------------------------------------------------

// sighting is what the browser remembers about one peer between events.
type sighting struct {
	device  protocol.Device
	seen    time.Time
	emitted time.Time
}

// browser holds the discovery state. Everything on it is touched only by the
// Browse goroutine, so it needs no lock.
type browser struct {
	logger *slog.Logger
	selfID string

	// known is keyed by TXT id, never by instance name or address. Two daemons
	// on one host share an address and a hostname and must still show up as two
	// devices; that is the normal way this gets tested.
	known map[string]*sighting

	pref   addrPref
	prefAt time.Time
	// refreshEvery is how long the local address view is reused before being
	// rebuilt from the live interface list. Zero pins it, which is what tests
	// want so that scoring does not depend on whatever adapters this machine
	// happens to have.
	refreshEvery time.Duration
}

// observe folds one browse result into the sighting table and reports the event
// a consumer should see, if any.
func (b *browser) observe(entry *zeroconf.ServiceEntry, now time.Time) (Event, bool) {
	if entry == nil {
		return Event{}, false
	}
	if isSelf(entry.Text, b.selfID) {
		return Event{}, false
	}

	id := parseTXT(entry.Text)[TXTKeyID]
	if id == "" {
		// Some other service answered, or a peer published a malformed record.
		// Either way there is no identity to key on: log and move on.
		b.logger.Debug("ignoring mDNS entry with no TXT id",
			"instance", entry.Instance, "host", entry.HostName)
		return Event{}, false
	}
	if entry.Port <= 0 || entry.Port > 65535 {
		b.logger.Debug("ignoring mDNS entry with an unusable SRV port",
			"deviceId", id, "port", entry.Port)
		return Event{}, false
	}

	// Refresh the local address view about once per cycle so a peer that appears
	// right after a Wi-Fi join is still scored against the current subnets.
	if b.refreshEvery > 0 && now.Sub(b.prefAt) >= b.refreshEvery {
		b.pref = localAddrPref()
		b.prefAt = now
	}

	addr := b.pref.pick(entry.AddrIPv4, entry.AddrIPv6)
	if addr == nil {
		b.logger.Debug("ignoring mDNS entry with no usable address",
			"deviceId", id, "ipv4", entry.AddrIPv4, "ipv6", entry.AddrIPv6)
		return Event{}, false
	}

	dev := DeviceFromTXT(entry.Text, addr, entry.Port, nowMillis(now))

	prev, known := b.known[id]
	if !known {
		b.known[id] = &sighting{device: dev, seen: now, emitted: now}
		b.logger.Info("device discovered",
			"deviceId", id, "name", dev.Name, "address", dev.Address, "peerPort", dev.Port,
			"platform", dev.Platform, "version", dev.Version)
		return Event{Kind: EventAdd, Device: dev, At: now}, true
	}

	changed := prev.device.Address != dev.Address ||
		prev.device.Port != dev.Port ||
		prev.device.Name != dev.Name ||
		prev.device.Version != dev.Version ||
		prev.device.Platform != dev.Platform ||
		prev.device.Status != dev.Status

	prev.device = dev
	prev.seen = now
	if !changed && now.Sub(prev.emitted) < emitRefreshAfter {
		return Event{}, false
	}
	prev.emitted = now
	if changed {
		b.logger.Info("device changed",
			"deviceId", id, "name", dev.Name, "address", dev.Address, "peerPort", dev.Port)
	}
	return Event{Kind: EventUpdate, Device: dev, At: now}, true
}

// sweep retires devices that have not been seen for DeviceTTL. mDNS goodbye
// packets are best-effort and a machine that is unplugged sends none at all, so
// expiry is the only removal path that always works.
func (b *browser) sweep(now time.Time) []Event {
	var expired []string
	for id, s := range b.known {
		if now.Sub(s.seen) > DeviceTTL {
			expired = append(expired, id)
		}
	}
	if len(expired) == 0 {
		return nil
	}
	sort.Strings(expired)

	events := make([]Event, 0, len(expired))
	for _, id := range expired {
		dev := b.known[id].device
		dev.Status = protocol.StatusOffline
		delete(b.known, id)
		b.logger.Info("device went offline",
			"deviceId", id, "name", dev.Name, "notSeenFor", DeviceTTL)
		events = append(events, Event{Kind: EventRemove, Device: dev, At: now})
	}
	return events
}

// -----------------------------------------------------------------------------
// address selection
// -----------------------------------------------------------------------------

// addrPref ranks a peer's advertised addresses against this machine's own view
// of the network. A host advertises every address it has, including virtual
// adapters and IPv6 link-local, and only some of them are dialable from here.
type addrPref struct {
	nets []*net.IPNet
	own  map[string]bool
	// primary is the subnet of the address the routing table would use to reach
	// the wider network, or nil when that cannot be determined. It is the tie
	// breaker that matters in practice: a developer box has a VirtualBox or WSL
	// adapter whose 192.168.56.0/24 overlaps the peer's own virtual adapter, so
	// "same subnet as one of ours" on its own happily picks an address that is
	// neither reachable nor the peer's LAN address.
	primary *net.IPNet
}

// primaryLocalIP asks the routing table which local address would be used to
// reach the wider network. The UDP socket sends nothing - connect on UDP only
// resolves a route - so this costs a syscall and no traffic. TEST-NET-1 is used
// as the target precisely because it is never routable anywhere.
func primaryLocalIP() net.IP {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return nil
	}
	defer c.Close()
	addr, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	return addr.IP
}

// localAddrPref snapshots the addresses and subnets of this machine.
func localAddrPref() addrPref {
	pref := addrPref{own: make(map[string]bool)}
	ifaces, err := net.Interfaces()
	if err != nil {
		return pref
	}
	primary := primaryLocalIP()
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP == nil {
				continue
			}
			pref.nets = append(pref.nets, n)
			pref.own[n.IP.String()] = true
			if primary != nil && n.Contains(primary) {
				pref.primary = n
			}
		}
	}
	return pref
}

// inLocalNet reports whether ip falls inside one of our own interface subnets,
// which is the strongest available hint that it is reachable from here.
func (p addrPref) inLocalNet(ip net.IP) bool {
	for _, n := range p.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// score ranks one candidate address. Higher is better; a negative score means
// unusable. IPv4 outranks IPv6 because LAN pairing happens on a single subnet
// where every host has one, and an IPv6 link-local address carries no zone in
// the DNS record so it usually cannot be dialed at all.
func (p addrPref) score(ip net.IP) int {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return -1
	}
	var base int
	switch {
	case ip.To4() != nil && !ip.IsLinkLocalUnicast():
		base = 8 // ordinary IPv4, the common case
	case ip.To4() != nil:
		base = 2 // 169.254/16 autoconfiguration
	case ip.IsGlobalUnicast():
		base = 4 // routable IPv6
	default:
		base = 1 // fe80::, needs a zone the record does not carry
	}
	if p.primary != nil && p.primary.Contains(ip) {
		// Same subnet as the interface this machine actually routes through.
		base += 4
	}
	if p.inLocalNet(ip) {
		base += 2
	}
	if !p.own[ip.String()] {
		// Prefer an address that is not literally ours: when a peer advertises
		// a virtual adapter that collides with one of our own, the other
		// candidate is far more likely to be the real one. A second daemon on
		// this host still scores above nothing, so same-machine testing works.
		base++
	}
	return base
}

// pick chooses the address a peer should be dialed on, preferring IPv4 and
// falling back to IPv6. It returns nil when nothing is usable.
func (p addrPref) pick(v4, v6 []net.IP) net.IP {
	best := -1
	var chosen net.IP
	for _, list := range [][]net.IP{v4, v6} {
		for _, ip := range list {
			if s := p.score(ip); s > best {
				best, chosen = s, ip
			}
		}
	}
	if best < 0 {
		return nil
	}
	return chosen
}
