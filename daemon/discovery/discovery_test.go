package discovery

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/zeroconf/v2"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testBrowser builds a browser whose address view is pinned, so scoring depends
// only on what the test supplies and not on this machine's adapters.
func testBrowser(selfID string, localCIDRs ...string) *browser {
	pref := addrPref{own: make(map[string]bool)}
	for _, cidr := range localCIDRs {
		ip, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("bad test CIDR " + cidr)
		}
		pref.nets = append(pref.nets, n)
		pref.own[ip.String()] = true
	}
	return &browser{
		logger: quietLogger(),
		selfID: selfID,
		known:  make(map[string]*sighting),
		pref:   pref,
		// refreshEvery stays zero: the pinned view above is never rebuilt.
	}
}

func entry(instance string, txt []string, port int, v4 ...string) *zeroconf.ServiceEntry {
	e := &zeroconf.ServiceEntry{
		ServiceRecord: zeroconf.ServiceRecord{
			Instance: instance,
			Service:  ServiceType,
			Domain:   Domain,
		},
		HostName: "host.local.",
		Port:     port,
		Text:     txt,
	}
	for _, a := range v4 {
		e.AddrIPv4 = append(e.AddrIPv4, net.ParseIP(a))
	}
	return e
}

// -----------------------------------------------------------------------------
// TXT encode / decode
// -----------------------------------------------------------------------------

func TestTXTRecordRoundTrip(t *testing.T) {
	self := protocol.SelfDevice{
		ID:       "9ca55654-2f0e-4d1a-b7c9-0f6a1e2b3c4d",
		Name:     "Aashish Laptop",
		Version:  "0.1.0",
		Platform: protocol.PlatformWindows,
	}

	txt := TXTRecord(self)
	if len(txt) != 5 {
		t.Fatalf("TXTRecord returned %d entries, want 5: %v", len(txt), txt)
	}

	// The wire form is what another daemon parses, so assert the exact strings
	// rather than only the round trip.
	want := []string{
		"id=9ca55654-2f0e-4d1a-b7c9-0f6a1e2b3c4d",
		"name=Aashish Laptop",
		"ver=0.1.0",
		"proto=2",
		"plat=windows",
	}
	for i, w := range want {
		if txt[i] != w {
			t.Errorf("TXTRecord[%d] = %q, want %q", i, txt[i], w)
		}
	}

	seen := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	got := DeviceFromTXT(txt, net.ParseIP("10.0.0.7"), protocol.DefaultPeerPort, nowMillis(seen))

	if got.ID != self.ID {
		t.Errorf("ID = %q, want %q", got.ID, self.ID)
	}
	if got.Name != self.Name {
		t.Errorf("Name = %q, want %q", got.Name, self.Name)
	}
	if got.Version != self.Version {
		t.Errorf("Version = %q, want %q", got.Version, self.Version)
	}
	if got.Platform != protocol.PlatformWindows {
		t.Errorf("Platform = %q, want %q", got.Platform, protocol.PlatformWindows)
	}
	if got.Address != "10.0.0.7" {
		t.Errorf("Address = %q, want %q", got.Address, "10.0.0.7")
	}
	if got.Port != protocol.DefaultPeerPort {
		t.Errorf("Port = %d, want the peer port %d", got.Port, protocol.DefaultPeerPort)
	}
	if got.Status != protocol.StatusOnline {
		t.Errorf("Status = %q, want %q", got.Status, protocol.StatusOnline)
	}
	if got.LastSeen != seen.UnixMilli() {
		t.Errorf("LastSeen = %d, want %d", got.LastSeen, seen.UnixMilli())
	}
	// Paired is a trust-store fact, never a network fact.
	if got.Paired {
		t.Error("Paired = true, want false: discovery must never claim a device is trusted")
	}
}

func TestTXTRecordFillsMissingFields(t *testing.T) {
	txt := TXTRecord(protocol.SelfDevice{ID: "abc", Name: "N"})
	joined := strings.Join(txt, " ")
	if !strings.Contains(joined, "plat="+protocol.PlatformUnknown) {
		t.Errorf("missing platform should render as %q, got %q", protocol.PlatformUnknown, joined)
	}
	if !strings.Contains(joined, "ver="+protocol.AppVersion) {
		t.Errorf("missing version should render as %q, got %q", protocol.AppVersion, joined)
	}
}

func TestDeviceFromTXTTolerantOfMalformedRecords(t *testing.T) {
	tests := []struct {
		name string
		txt  []string
		want protocol.Device
	}{
		{
			name: "no equals sign is skipped",
			txt:  []string{"id=a1", "garbage", "name=Box"},
			want: protocol.Device{ID: "a1", Name: "Box"},
		},
		{
			name: "empty key is skipped",
			txt:  []string{"=orphan", "id=a2"},
			want: protocol.Device{ID: "a2"},
		},
		{
			name: "first occurrence of a key wins",
			txt:  []string{"id=first", "id=second"},
			want: protocol.Device{ID: "first"},
		},
		{
			name: "empty value is preserved",
			txt:  []string{"id=a3", "name="},
			want: protocol.Device{ID: "a3", Name: ""},
		},
		{
			name: "unrecognised platform falls back to unknown",
			txt:  []string{"id=a4", "plat=freebsd"},
			want: protocol.Device{ID: "a4"},
		},
		{
			name: "no TXT at all",
			txt:  nil,
			want: protocol.Device{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeviceFromTXT(tc.txt, nil, 0, 0)
			if got.ID != tc.want.ID {
				t.Errorf("ID = %q, want %q", got.ID, tc.want.ID)
			}
			if got.Name != tc.want.Name {
				t.Errorf("Name = %q, want %q", got.Name, tc.want.Name)
			}
			if got.Platform != protocol.PlatformUnknown {
				t.Errorf("Platform = %q, want %q", got.Platform, protocol.PlatformUnknown)
			}
			if got.Address != "" {
				t.Errorf("Address = %q, want empty for a nil IP", got.Address)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// self-filter predicate
// -----------------------------------------------------------------------------

func TestIsSelf(t *testing.T) {
	const selfID = "9ca55654-2f0e-4d1a-b7c9-0f6a1e2b3c4d"
	const otherID = "11111111-2222-3333-4444-555555555555"

	tests := []struct {
		name   string
		txt    []string
		selfID string
		want   bool
	}{
		{
			name:   "our own advertisement",
			txt:    TXTRecord(protocol.SelfDevice{ID: selfID, Name: "Mine"}),
			selfID: selfID,
			want:   true,
		},
		{
			name:   "another device",
			txt:    TXTRecord(protocol.SelfDevice{ID: otherID, Name: "Theirs"}),
			selfID: selfID,
			want:   false,
		},
		{
			name: "same instance name and host, different id, is not us",
			// Two daemons on one machine share hostname and every address, and
			// a second machine can be named the same thing. Only the id counts.
			txt:    []string{"name=Aashish Laptop", "id=" + otherID},
			selfID: selfID,
			want:   false,
		},
		{
			name:   "id key order does not matter",
			txt:    []string{"plat=linux", "name=Mine", "id=" + selfID},
			selfID: selfID,
			want:   true,
		},
		{
			name:   "no id in the record is never us",
			txt:    []string{"name=Mystery", "ver=0.1.0"},
			selfID: selfID,
			want:   false,
		},
		{
			name:   "malformed record is never us",
			txt:    []string{"idnoequals", "=", selfID},
			selfID: selfID,
			want:   false,
		},
		{
			name:   "an empty self id filters nothing rather than everything",
			txt:    []string{"id=" + otherID},
			selfID: "",
			want:   false,
		},
		{
			name:   "an empty self id does not match an empty advertised id",
			txt:    []string{"id="},
			selfID: "",
			want:   false,
		},
		{
			name:   "id is compared exactly, not by prefix",
			txt:    []string{"id=" + selfID + "-extra"},
			selfID: selfID,
			want:   false,
		},
		{
			name:   "nil TXT",
			txt:    nil,
			selfID: selfID,
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSelf(tc.txt, tc.selfID); got != tc.want {
				t.Errorf("isSelf(%v, %q) = %v, want %v", tc.txt, tc.selfID, got, tc.want)
			}
		})
	}
}

func TestObserveDropsSelf(t *testing.T) {
	const selfID = "self-1"
	b := testBrowser(selfID, "10.0.0.1/24")

	txt := TXTRecord(protocol.SelfDevice{ID: selfID, Name: "Mine", Platform: protocol.PlatformWindows})
	if _, emit := b.observe(entry("Mine-self", txt, protocol.DefaultPeerPort, "10.0.0.1"), time.Now()); emit {
		t.Fatal("observe emitted an event for our own advertisement")
	}
	if len(b.known) != 0 {
		t.Fatalf("self advertisement was recorded: %v", b.known)
	}
}

// -----------------------------------------------------------------------------
// liveness tracking
// -----------------------------------------------------------------------------

func TestObserveAddThenUpdate(t *testing.T) {
	b := testBrowser("self-1", "10.0.0.1/24")
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	txt := TXTRecord(protocol.SelfDevice{ID: "peer-1", Name: "Peer One", Version: "0.1.0", Platform: protocol.PlatformLinux})
	ev, emit := b.observe(entry("Peer-One-peer1", txt, 8766, "10.0.0.5"), now)
	if !emit || ev.Kind != EventAdd {
		t.Fatalf("first sighting: kind=%q emit=%v, want an add", ev.Kind, emit)
	}
	if ev.Device.Address != "10.0.0.5" || ev.Device.Port != 8766 {
		t.Fatalf("added device = %s:%d, want 10.0.0.5:8766", ev.Device.Address, ev.Device.Port)
	}

	// An immediate identical re-sighting is noise and must stay silent.
	if _, emit := b.observe(entry("Peer-One-peer1", txt, 8766, "10.0.0.5"), now.Add(time.Second)); emit {
		t.Error("an unchanged re-sighting inside emitRefreshAfter emitted an event")
	}

	// A changed port is a real change and must surface at once.
	ev, emit = b.observe(entry("Peer-One-peer1", txt, 9000, "10.0.0.5"), now.Add(2*time.Second))
	if !emit || ev.Kind != EventUpdate {
		t.Fatalf("changed port: kind=%q emit=%v, want an update", ev.Kind, emit)
	}
	if ev.Device.Port != 9000 {
		t.Errorf("updated port = %d, want 9000", ev.Device.Port)
	}

	// Nothing changed, but the consumer copy of LastSeen has gone stale.
	later := now.Add(2*time.Second + emitRefreshAfter)
	ev, emit = b.observe(entry("Peer-One-peer1", txt, 9000, "10.0.0.5"), later)
	if !emit || ev.Kind != EventUpdate {
		t.Fatalf("stale refresh: kind=%q emit=%v, want an update", ev.Kind, emit)
	}
	if ev.Device.LastSeen != later.UnixMilli() {
		t.Errorf("refreshed LastSeen = %d, want %d", ev.Device.LastSeen, later.UnixMilli())
	}
}

func TestObserveTwoDaemonsOnOneHost(t *testing.T) {
	// This is not a corner case: it is how LAN pairing gets tested on a single
	// machine. Same hostname, same addresses, two ids, two devices.
	b := testBrowser("self-1", "10.0.0.1/24")
	now := time.Now()

	a := TXTRecord(protocol.SelfDevice{ID: "peer-a", Name: "Box", Platform: protocol.PlatformWindows})
	c := TXTRecord(protocol.SelfDevice{ID: "peer-b", Name: "Box", Platform: protocol.PlatformWindows})

	evA, emitA := b.observe(entry("Box-peera", a, 8766, "10.0.0.9"), now)
	evB, emitB := b.observe(entry("Box-peerb", c, 8767, "10.0.0.9"), now)

	if !emitA || !emitB {
		t.Fatalf("both daemons must be reported: emitA=%v emitB=%v", emitA, emitB)
	}
	if evA.Kind != EventAdd || evB.Kind != EventAdd {
		t.Fatalf("kinds = %q, %q, want two adds", evA.Kind, evB.Kind)
	}
	if len(b.known) != 2 {
		t.Fatalf("tracked %d devices, want 2: %v", len(b.known), b.known)
	}
	if evA.Device.Address != evB.Device.Address {
		t.Fatalf("addresses differ (%s vs %s); the fixture is wrong",
			evA.Device.Address, evB.Device.Address)
	}
	if evA.Device.Port == evB.Device.Port {
		t.Errorf("both devices resolved to port %d; the SRV port must be per-instance", evA.Device.Port)
	}
}

func TestSweepRemovesAfterTTL(t *testing.T) {
	b := testBrowser("self-1", "10.0.0.1/24")
	now := time.Now()

	txt := TXTRecord(protocol.SelfDevice{ID: "peer-1", Name: "Peer One"})
	if _, emit := b.observe(entry("Peer-One", txt, 8766, "10.0.0.5"), now); !emit {
		t.Fatal("setup: first sighting was not emitted")
	}

	if evs := b.sweep(now.Add(DeviceTTL)); len(evs) != 0 {
		t.Fatalf("device removed at exactly DeviceTTL: %v", evs)
	}

	evs := b.sweep(now.Add(DeviceTTL + time.Second))
	if len(evs) != 1 {
		t.Fatalf("sweep returned %d events, want 1", len(evs))
	}
	if evs[0].Kind != EventRemove {
		t.Errorf("kind = %q, want %q", evs[0].Kind, EventRemove)
	}
	if evs[0].Device.ID != "peer-1" {
		t.Errorf("removed device id = %q, want %q", evs[0].Device.ID, "peer-1")
	}
	if evs[0].Device.Status != protocol.StatusOffline {
		t.Errorf("removed device status = %q, want %q", evs[0].Device.Status, protocol.StatusOffline)
	}
	if len(b.known) != 0 {
		t.Errorf("removed device is still tracked: %v", b.known)
	}

	// A device that comes back is an add again, not an update of a ghost.
	ev, emit := b.observe(entry("Peer-One", txt, 8766, "10.0.0.5"), now.Add(2*DeviceTTL))
	if !emit || ev.Kind != EventAdd {
		t.Fatalf("returning device: kind=%q emit=%v, want an add", ev.Kind, emit)
	}
}

func TestObserveRejectsUnusableEntries(t *testing.T) {
	b := testBrowser("self-1", "10.0.0.1/24")
	now := time.Now()
	good := TXTRecord(protocol.SelfDevice{ID: "peer-1", Name: "Peer"})

	cases := []struct {
		name  string
		entry *zeroconf.ServiceEntry
	}{
		{"nil entry", nil},
		{"no TXT id", entry("x", []string{"name=Nameless"}, 8766, "10.0.0.5")},
		{"malformed TXT only", entry("x", []string{"not-a-pair"}, 8766, "10.0.0.5")},
		{"zero SRV port", entry("x", good, 0, "10.0.0.5")},
		{"out of range SRV port", entry("x", good, 70000, "10.0.0.5")},
		{"no address", entry("x", good, 8766)},
		{"loopback only", entry("x", good, 8766, "127.0.0.1")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, emit := b.observe(tc.entry, now); emit {
				t.Error("emitted an event for an unusable entry")
			}
		})
	}
	if len(b.known) != 0 {
		t.Errorf("unusable entries were tracked: %v", b.known)
	}
}

// -----------------------------------------------------------------------------
// address selection
// -----------------------------------------------------------------------------

func TestAddrPrefPick(t *testing.T) {
	_, lan, err := net.ParseCIDR("10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	_, virt, err := net.ParseCIDR("192.168.56.0/24")
	if err != nil {
		t.Fatal(err)
	}
	pref := addrPref{
		nets: []*net.IPNet{lan, virt},
		own:  map[string]bool{"10.0.0.1": true, "192.168.56.1": true},
	}

	ips := func(s ...string) []net.IP {
		out := make([]net.IP, 0, len(s))
		for _, one := range s {
			out = append(out, net.ParseIP(one))
		}
		return out
	}

	tests := []struct {
		name string
		v4   []net.IP
		v6   []net.IP
		want string
	}{
		{
			name: "our own subnet beats a foreign one",
			v4:   ips("172.20.5.9", "10.0.0.5"),
			want: "10.0.0.5",
		},
		{
			name: "a peer that is not literally us beats a colliding virtual adapter",
			v4:   ips("192.168.56.1", "10.0.0.5"),
			want: "10.0.0.5",
		},
		{
			name: "a second daemon on this host still resolves to a usable address",
			v4:   ips("10.0.0.1"),
			want: "10.0.0.1",
		},
		{
			name: "loopback and unspecified are never chosen",
			v4:   ips("127.0.0.1", "0.0.0.0", "172.20.5.9"),
			want: "172.20.5.9",
		},
		{
			name: "IPv4 outranks IPv6",
			v4:   ips("172.20.5.9"),
			v6:   ips("2001:db8::1"),
			want: "172.20.5.9",
		},
		{
			name: "IPv6 is used when there is no IPv4",
			v6:   ips("2001:db8::1"),
			want: "2001:db8::1",
		},
		{
			name: "routable IPv6 beats link-local IPv6",
			v6:   ips("fe80::1", "2001:db8::1"),
			want: "2001:db8::1",
		},
		{
			name: "an APIPA address is better than nothing",
			v4:   ips("169.254.3.4"),
			want: "169.254.3.4",
		},
	}

	// The routed subnet outranks a virtual adapter that merely overlaps one of
	// ours, which is the case that actually goes wrong on a developer machine.
	routed := addrPref{
		nets:    []*net.IPNet{lan, virt},
		own:     map[string]bool{"10.0.0.1": true, "192.168.56.1": true},
		primary: lan,
	}
	if got := routed.pick(ips("192.168.56.101", "10.0.0.5"), nil); got.String() != "10.0.0.5" {
		t.Errorf("pick = %s, want 10.0.0.5: the routed subnet must win", got)
	}
	if got := routed.pick(ips("192.168.56.101"), nil); got.String() != "192.168.56.101" {
		t.Errorf("pick = %s, want 192.168.56.101 when it is the only candidate", got)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pref.pick(tc.v4, tc.v6)
			if got == nil {
				t.Fatalf("pick returned nil, want %s", tc.want)
			}
			if got.String() != tc.want {
				t.Errorf("pick = %s, want %s", got, tc.want)
			}
		})
	}

	if got := pref.pick(nil, nil); got != nil {
		t.Errorf("pick with no candidates = %s, want nil", got)
	}
	if got := pref.pick(ips("127.0.0.1"), nil); got != nil {
		t.Errorf("pick with only loopback = %s, want nil", got)
	}
}

// -----------------------------------------------------------------------------
// instance naming
// -----------------------------------------------------------------------------

func TestInstanceName(t *testing.T) {
	tests := []struct {
		name string
		self protocol.SelfDevice
		want string
	}{
		{
			name: "spec example",
			self: protocol.SelfDevice{Name: "Aashish Laptop", ID: "9ca55654-2f0e-4d1a-b7c9-0f6a1e2b3c4d"},
			want: "Aashish-Laptop-9ca55654",
		},
		{
			name: "punctuation collapses to single hyphens",
			self: protocol.SelfDevice{Name: "Bob's  MacBook Pro (14\")", ID: "abcdef01-2345"},
			want: "Bob-s-MacBook-Pro-14-abcdef01",
		},
		{
			name: "a name that sanitises to nothing falls back",
			self: protocol.SelfDevice{Name: "???", ID: "abcdef01-2345"},
			want: instanceFallbackName + "-abcdef01",
		},
		{
			name: "short id is used whole",
			self: protocol.SelfDevice{Name: "Box", ID: "ab"},
			want: "Box-ab",
		},
		{
			name: "no id at all",
			self: protocol.SelfDevice{Name: "Box"},
			want: "Box",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := instanceName(tc.self); got != tc.want {
				t.Errorf("instanceName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInstanceNameFitsADNSLabel(t *testing.T) {
	self := protocol.SelfDevice{
		Name: strings.Repeat("VeryLongDeviceName", 10),
		ID:   "9ca55654-2f0e-4d1a-b7c9-0f6a1e2b3c4d",
	}
	got := instanceName(self)
	if len(got) > 63 {
		t.Errorf("instanceName is %d bytes, which overflows a DNS label: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "-9ca55654") {
		t.Errorf("truncation dropped the id suffix: %q", got)
	}
}

// -----------------------------------------------------------------------------
// argument validation
// -----------------------------------------------------------------------------

func TestAdvertiseRejectsBadArguments(t *testing.T) {
	self := protocol.SelfDevice{ID: "abc", Name: "Box", Platform: protocol.PlatformWindows}

	if _, err := Advertise(context.Background(), quietLogger(), protocol.SelfDevice{Name: "Box"}, 8766); err == nil {
		t.Error("advertising a device with no id succeeded")
	}
	if _, err := Advertise(context.Background(), quietLogger(), self, 0); err == nil {
		t.Error("advertising on port 0 succeeded")
	}
	if _, err := Advertise(context.Background(), quietLogger(), self, 70000); err == nil {
		t.Error("advertising on an out-of-range port succeeded")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Advertise(ctx, quietLogger(), self, 8766); err == nil {
		t.Error("advertising with a cancelled context succeeded")
	}
}

func TestBrowseRejectsNilChannel(t *testing.T) {
	if err := Browse(context.Background(), quietLogger(), "self", nil); err == nil {
		t.Fatal("Browse with a nil channel returned no error")
	}
}

func TestBrowseClosesChannelOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	out := make(chan Event, 8)
	done := make(chan error, 1)
	go func() { done <- Browse(ctx, quietLogger(), "self-id", out) }()

	// Draining is what a real consumer does; it also proves out gets closed.
	for range out {
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Browse returned %v, want nil for a cancelled context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Browse did not return after its channel closed")
	}
}

// -----------------------------------------------------------------------------
// live network round trip, opt-in
// -----------------------------------------------------------------------------

// TestLiveAdvertiseAndBrowse exercises the real multicast path: it advertises two
// identities and browses as one of them, so both the discovery of a peer and the
// self-filter are checked over the wire. It is opt-in because it needs a
// multicast-capable interface and, on Windows, an allowed firewall rule.
//
//	SHARING_MDNS_LIVE=1 go test ./discovery -run Live -v
func TestLiveAdvertiseAndBrowse(t *testing.T) {
	if os.Getenv("SHARING_MDNS_LIVE") != "1" {
		t.Skip("set SHARING_MDNS_LIVE=1 to run the multicast round trip")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	selfDev := protocol.SelfDevice{ID: "live-self-0001", Name: "Live Self", Version: "0.1.0", Platform: protocol.PlatformWindows}
	peerDev := protocol.SelfDevice{ID: "live-peer-0002", Name: "Live Peer", Version: "0.1.0", Platform: protocol.PlatformLinux}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfAd, err := Advertise(ctx, logger, selfDev, 18765)
	if err != nil {
		t.Skipf("cannot advertise on this host: %v", err)
	}
	defer selfAd.Close()

	peerAd, err := Advertise(ctx, logger, peerDev, 18766)
	if err != nil {
		t.Skipf("cannot advertise a second instance on this host: %v", err)
	}
	defer peerAd.Close()

	out := make(chan Event, 32)
	go func() { _ = Browse(ctx, logger, selfDev.ID, out) }()

	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-out:
			if !ok {
				t.Fatal("browse channel closed before the peer was seen")
			}
			if ev.Device.ID == selfDev.ID {
				t.Fatalf("self-filter leaked our own advertisement: %+v", ev.Device)
			}
			if ev.Device.ID != peerDev.ID {
				continue // some other daemon on this network
			}
			if ev.Kind != EventAdd {
				continue
			}
			if ev.Device.Port != 18766 {
				t.Errorf("peer port = %d, want the advertised peer port 18766", ev.Device.Port)
			}
			if ev.Device.Name != peerDev.Name {
				t.Errorf("peer name = %q, want %q", ev.Device.Name, peerDev.Name)
			}
			if ev.Device.Platform != protocol.PlatformLinux {
				t.Errorf("peer platform = %q, want %q", ev.Device.Platform, protocol.PlatformLinux)
			}
			if ev.Device.Address == "" {
				t.Error("peer address is empty")
			}
			return
		case <-deadline:
			t.Fatal("the second advertisement was never discovered within 15s")
		}
	}
}
