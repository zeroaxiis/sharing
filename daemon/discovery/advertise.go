package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// ErrNoIdentity is returned when a caller tries to advertise a device with no
// UUID. The TXT id is the only key peers have; publishing without one would
// create a record nobody can address or self-filter.
var ErrNoIdentity = errors.New("discovery: device has no id")

// ErrInvalidPort is returned for a peer port outside the TCP range.
var ErrInvalidPort = errors.New("discovery: peer port out of range")

// advertiser owns one zeroconf registration and the goroutine that re-creates it
// when the machine network configuration changes.
type advertiser struct {
	logger   *slog.Logger
	instance string
	txt      []string
	port     int

	stop     context.CancelFunc
	watchers sync.WaitGroup

	mu sync.Mutex
	// srv is nil between a failed re-registration and the next successful one.
	srv    *zeroconf.Server
	closed bool

	closeOnce sync.Once
}

// Advertise publishes this daemon on the local network so peers can find it.
//
// peerPort is the LAN peer listener port and is what lands in the SRV record.
// The control port is never advertised: it is bound to loopback and a peer that
// tried to dial it would be talking to its own machine.
//
// The returned Advertiser stays registered until Close is called or ctx is done,
// whichever happens first. While it is alive the advertisement is re-created
// whenever the set of up, multicast-capable interfaces and their addresses
// changes, so joining Wi-Fi or dropping a VPN does not leave the daemon
// advertising on interfaces that no longer exist.
func Advertise(ctx context.Context, logger *slog.Logger, self protocol.SelfDevice, peerPort int) (Advertiser, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if self.ID == "" {
		return nil, fmt.Errorf("discovery: advertise: %w", ErrNoIdentity)
	}
	if peerPort <= 0 || peerPort > 65535 {
		return nil, fmt.Errorf("discovery: advertise on port %d: %w", peerPort, ErrInvalidPort)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("discovery: advertise: %w", err)
	}

	a := &advertiser{
		logger:   logger,
		instance: instanceName(self),
		txt:      TXTRecord(self),
		port:     peerPort,
	}

	logFirewallNotice(logger)

	srv, err := a.register()
	if err != nil {
		return nil, err
	}
	a.srv = srv

	logger.Info("mDNS advertisement registered",
		"instance", a.instance,
		"serviceType", ServiceType,
		"domain", Domain,
		"peerPort", peerPort,
		"txt", strings.Join(a.txt, " "),
	)

	watchCtx, cancel := context.WithCancel(ctx)
	a.stop = cancel
	a.watchers.Add(1)
	go a.watch(watchCtx)

	return a, nil
}

// register performs one zeroconf registration with the current interface set.
// Passing nil interfaces makes zeroconf enumerate them itself, which is what
// makes a re-registration pick up a newly connected adapter.
func (a *advertiser) register() (*zeroconf.Server, error) {
	srv, err := zeroconf.Register(a.instance, ServiceType, Domain, a.port, a.txt, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery: register %q as %s in %s on port %d: %w",
			a.instance, ServiceType, Domain, a.port, err)
	}
	return srv, nil
}

// watch shuts the advertisement down when ctx ends, and until then re-registers
// it whenever the interface fingerprint changes.
func (a *advertiser) watch(ctx context.Context) {
	defer a.watchers.Done()

	fingerprint := interfaceFingerprint()
	ticker := time.NewTicker(interfaceWatch)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Close is safe to call from here: it is guarded by sync.Once, so a
			// caller closing at the same time as a cancelled context does not
			// double-shutdown the responder.
			_ = a.closeServer()
			return
		case <-ticker.C:
			current := interfaceFingerprint()
			if current == fingerprint && a.healthy() {
				continue
			}
			a.reregister(current, &fingerprint)
		}
	}
}

// healthy reports whether a registration is currently live. It is false only
// between a failed re-registration and the next attempt.
func (a *advertiser) healthy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.srv != nil && !a.closed
}

// reregister replaces the live registration. On failure it leaves the advertiser
// unregistered and does not adopt the new fingerprint, so the next tick retries.
func (a *advertiser) reregister(current string, fingerprint *string) {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	old := a.srv
	a.srv = nil
	a.mu.Unlock()

	if old != nil {
		old.Shutdown()
	}

	srv, err := a.register()
	if err != nil {
		a.logger.Warn("mDNS re-advertisement failed, retrying", "instance", a.instance, "error", err)
		return
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		srv.Shutdown()
		return
	}
	a.srv = srv
	a.mu.Unlock()

	*fingerprint = current
	a.logger.Info("network interfaces changed, mDNS advertisement re-registered",
		"instance", a.instance, "peerPort", a.port)
}

// Close withdraws the advertisement and stops the interface watcher. It is
// idempotent and safe to call concurrently with context cancellation.
func (a *advertiser) Close() error {
	a.closeOnce.Do(func() {
		if a.stop != nil {
			a.stop()
		}
		a.watchers.Wait()
		_ = a.closeServer()
		a.logger.Debug("mDNS advertisement withdrawn", "instance", a.instance)
	})
	return nil
}

// closeServer shuts the responder down at most once, sending goodbye packets so
// peers drop this device immediately instead of waiting out DeviceTTL.
func (a *advertiser) closeServer() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	srv := a.srv
	a.srv = nil
	a.mu.Unlock()

	if srv != nil {
		srv.Shutdown()
	}
	return nil
}

// interfaceFingerprint summarises the interfaces mDNS can actually use: up,
// multicast-capable, and their addresses. Comparing two fingerprints answers
// "did the network under us change?" without depending on a platform-specific
// change notification API.
//
// An error enumerating interfaces yields the empty string, which compares unequal
// to any real fingerprint and so triggers a retry rather than a silent stall.
func interfaceFingerprint() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(ifaces))
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		names := make([]string, 0, len(addrs))
		for _, addr := range addrs {
			names = append(names, addr.String())
		}
		sort.Strings(names)
		parts = append(parts, ifi.Name+"="+strings.Join(names, ","))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}
