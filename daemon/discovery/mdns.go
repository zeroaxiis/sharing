// Package discovery finds other Sharing daemons on the local network over
// mDNS / DNS-SD, and advertises this daemon so they can find it back.
//
// The advertised SRV port is the PEER port (8766 by default), never the control
// port: the control listener is pinned to loopback and is unreachable from the
// LAN, so publishing it would only mislead peers.
//
// Identity lives entirely in the TXT record. Instance names and IP addresses are
// never used to decide who a result belongs to: one machine legitimately has
// several addresses, two machines can share a hostname, and two daemons on the
// same host share every address they have. The TXT "id" is the only key.
package discovery

import (
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// DNS-SD coordinates for the Sharing service. These are aliases of the
// protocol constants so the wire strings have exactly one definition.
const (
	// ServiceType is the DNS-SD service type advertised and browsed for.
	ServiceType = protocol.ServiceType
	// Domain is the multicast DNS domain. The trailing dot is required.
	Domain = protocol.Domain
)

// TXT record keys carried in the service advertisement.
const (
	TXTKeyID       = protocol.TXTKeyID       // device UUID
	TXTKeyName     = protocol.TXTKeyName     // human-readable device name
	TXTKeyVersion  = protocol.TXTKeyVersion  // daemon version, e.g. "0.1.0"
	TXTKeyProtocol = protocol.TXTKeyProtocol // wire protocol version, e.g. "2"
	TXTKeyPlatform = protocol.TXTKeyPlatform // windows | darwin | linux
)

// Liveness and cadence.
const (
	// DeviceTTL is how long a device stays online after its last sighting. Past
	// this, Browse emits EventRemove. It is protocol.DeviceOnlineTTL; the alias
	// exists so the timeout reads as a named constant at every use site.
	DeviceTTL = protocol.DeviceOnlineTTL

	// browseCycle is how long one underlying zeroconf browse runs before it is
	// torn down and restarted.
	//
	// The restart is load-bearing, not housekeeping. zeroconf de-duplicates:
	// once it has handed an entry to the caller it stays silent about that
	// instance until the record TTL expires, which for the default announcement
	// is around 53 minutes. A single long-lived browse would therefore report
	// each peer exactly once and never again, and every peer would look dead
	// DeviceTTL later. A fresh client per cycle re-queries and re-reports, and
	// that is what actually drives liveness here.
	browseCycle = 10 * time.Second

	// livenessTick is how often the sighting table is swept for expiry.
	livenessTick = 5 * time.Second

	// emitRefreshAfter bounds how stale a downstream copy of LastSeen may get.
	// A re-sighting that changes nothing visible is still re-emitted as
	// EventUpdate once the last emission for that device is older than this, so
	// staleness checks downstream stay honest even if browseCycle is raised.
	emitRefreshAfter = DeviceTTL / 3

	// interfaceWatch is how often the advertiser re-checks the interface set so
	// it can re-register after a Wi-Fi join, VPN toggle or dock event.
	interfaceWatch = 30 * time.Second

	// retryBackoffMin and retryBackoffMax bound the wait after a failed browse
	// cycle. Failure here is usually "no multicast interface yet", which fixes
	// itself when the user joins a network, so browsing retries forever rather
	// than giving up.
	retryBackoffMin = 2 * time.Second
	retryBackoffMax = 30 * time.Second
)

// Instance naming.
const (
	// instanceIDSuffixLen is how many leading characters of the device UUID are
	// appended to the instance name to disambiguate two daemons that happen to
	// have picked the same human-readable name.
	instanceIDSuffixLen = 8
	// instanceNameMaxLen caps the sanitised name portion. A DNS label is 63
	// bytes; this leaves room for the separator and the id suffix.
	instanceNameMaxLen = 40
	// instanceFallbackName is used when a device name sanitises to nothing.
	instanceFallbackName = "sharing"
)

// EventKind distinguishes the three things that can happen to a peer.
type EventKind string

const (
	// EventAdd is a device observed for the first time.
	EventAdd EventKind = "add"
	// EventUpdate is a re-sighting of a known device. It fires on refreshed
	// sightings, not only when a field changed, so a consumer that mirrors
	// these events always holds a current LastSeen. Treat it as idempotent.
	EventUpdate EventKind = "update"
	// EventRemove is a device that has not been seen for DeviceTTL.
	EventRemove EventKind = "remove"
)

// Event is what Browse emits. Device is fully populated on add and update. On
// remove only Device.ID is guaranteed; in practice the last known snapshot is
// supplied with Status set to protocol.StatusOffline.
type Event struct {
	Kind   EventKind
	Device protocol.Device
	At     time.Time
}

// Advertiser is a live mDNS service registration. Close withdraws the
// advertisement, which means sending goodbye packets and shutting the responder
// down. Close is idempotent.
type Advertiser interface {
	// Close withdraws the advertisement.
	Close() error
}

// TXTRecord renders a device identity as the DNS-SD TXT key=value slice that
// zeroconf.Register expects.
func TXTRecord(self protocol.SelfDevice) []string {
	platform := self.Platform
	if platform == "" {
		platform = protocol.PlatformUnknown
	}
	version := self.Version
	if version == "" {
		version = protocol.AppVersion
	}
	return []string{
		TXTKeyID + "=" + self.ID,
		TXTKeyName + "=" + self.Name,
		TXTKeyVersion + "=" + version,
		TXTKeyProtocol + "=" + strconv.Itoa(protocol.ProtocolVersion),
		TXTKeyPlatform + "=" + platform,
	}
}

// parseTXT splits DNS-SD key=value strings into a map. Anything without an "="
// or with an empty key is dropped: TXT records arrive from the network, so a
// malformed one must be skipped, never trusted and never fatal. Per RFC 6763 the
// first occurrence of a key wins.
func parseTXT(txt []string) map[string]string {
	fields := make(map[string]string, len(txt))
	for _, kv := range txt {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		key := kv[:i]
		if _, dup := fields[key]; dup {
			continue
		}
		fields[key] = kv[i+1:]
	}
	return fields
}

// DeviceFromTXT reassembles a peer from a browse result: the TXT key=value pairs
// plus the address and port taken from the A/AAAA and SRV records.
//
// Paired is deliberately left false. Whether a device is trusted is the trust
// store's answer, never the network's; the server stamps it from
// config.TrustStore.PairedIDs.
func DeviceFromTXT(txt []string, addr net.IP, port int, lastSeen int64) protocol.Device {
	fields := parseTXT(txt)

	address := ""
	if addr != nil {
		address = addr.String()
	}

	platform := fields[TXTKeyPlatform]
	switch platform {
	case protocol.PlatformWindows, protocol.PlatformDarwin, protocol.PlatformLinux:
	default:
		platform = protocol.PlatformUnknown
	}

	return protocol.Device{
		ID:       fields[TXTKeyID],
		Name:     fields[TXTKeyName],
		Address:  address,
		Port:     port,
		Platform: platform,
		Version:  fields[TXTKeyVersion],
		Status:   protocol.StatusOnline,
		LastSeen: lastSeen,
	}
}

// isSelf reports whether a browse result is this daemon's own advertisement.
//
// The test is TXT id equality and nothing else. Filtering on instance name would
// break the moment two devices are both called "Laptop"; filtering on IP would
// hide a second daemon running on this very host, which is exactly the
// configuration LAN pairing gets tested in. An empty selfID never matches, so a
// daemon with no identity filters nothing rather than everything.
func isSelf(txt []string, selfID string) bool {
	if selfID == "" {
		return false
	}
	return parseTXT(txt)[TXTKeyID] == selfID
}

// sanitizeInstance reduces a human device name to characters that survive a DNS
// label intact: letters, digits and single hyphens.
func sanitizeInstance(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > instanceNameMaxLen {
		s = strings.Trim(s[:instanceNameMaxLen], "-")
	}
	return s
}

// instanceName builds the DNS-SD instance name: a sanitised device name plus a
// short id suffix, e.g. "Aashish-Laptop-9ca55654". The suffix is what keeps two
// identically named devices from colliding on one subnet.
func instanceName(self protocol.SelfDevice) string {
	name := sanitizeInstance(self.Name)
	if name == "" {
		name = instanceFallbackName
	}
	suffix := sanitizeInstance(self.ID)
	if len(suffix) > instanceIDSuffixLen {
		suffix = suffix[:instanceIDSuffixLen]
	}
	if suffix == "" {
		return name
	}
	return name + "-" + suffix
}

// firewallNoticeOnce keeps the Windows firewall advice to a single line per
// process even though both the advertiser and the browser bind UDP 5353.
var firewallNoticeOnce sync.Once

// logFirewallNotice warns, once, that Windows interposes a firewall prompt in
// front of mDNS. A denied prompt produces no error anywhere in this package:
// packets simply never arrive, which is indistinguishable from an empty network.
// Saying so up front is the only way a user can tell the two apart.
func logFirewallNotice(logger *slog.Logger) {
	if runtime.GOOS != "windows" {
		return
	}
	firewallNoticeOnce.Do(func() {
		logger.Info("binding UDP 5353 for mDNS discovery: Windows Defender Firewall shows an allow/block prompt on first run, and it must be allowed for Private networks - a blocked prompt is silent and looks exactly like 'no devices found'",
			"port", 5353,
			"transport", "udp",
			"serviceType", ServiceType,
		)
	})
}

// nowMillis is the epoch-millisecond form used by protocol.Device.LastSeen.
func nowMillis(t time.Time) int64 { return t.UnixMilli() }
