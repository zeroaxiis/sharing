// Package discovery finds other Nearby Share daemons on the local network over
// mDNS / DNS-SD.
//
// TODO(M4): implement on top of github.com/libp2p/zeroconf/v2. The exported
// surface below is already shaped for that library so wiring it up is a body
// change only, never a signature change:
//
//	zeroconf.Register(instance, ServiceType, Domain, port, txt, ifaces) (*zeroconf.Server, error)
//	zeroconf.Browse(ctx, ServiceType, Domain, entries chan<- *zeroconf.ServiceEntry, opts...) error
//
// zeroconf is intentionally absent from go.mod until this package actually
// calls it, so the module graph stays honest about what the daemon links in.
package discovery

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strconv"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// DNS-SD coordinates for the Nearby Share service.
const (
	// ServiceType is the DNS-SD service type advertised and browsed for.
	ServiceType = "_nearby-share._tcp"
	// Domain is the multicast DNS domain. The trailing dot is required.
	Domain = "local."
)

// TXT record keys carried in the service advertisement.
const (
	TXTKeyID       = "id"    // device UUID
	TXTKeyName     = "name"  // human-readable device name
	TXTKeyVersion  = "ver"   // daemon version, e.g. "0.1.0"
	TXTKeyProtocol = "proto" // wire protocol version, e.g. "1"
)

// ErrNotImplemented is returned by every function in this package until
// Milestone 4 lands.
var ErrNotImplemented = errors.New("discovery: mDNS not implemented yet (M4)")

// Advertiser is a live mDNS service registration. Close withdraws the
// advertisement, which in the zeroconf implementation means sending goodbye
// packets and shutting the responder down.
type Advertiser interface {
	// Close withdraws the advertisement.
	Close() error
}

// Advertise publishes this daemon on the local network so peers can find it.
//
// self supplies the TXT record contents and the service instance name; port is
// the daemon loopback control port that peers will read out of the SRV record.
// The returned Advertiser stays registered until it is closed or ctx is done.
//
// TODO(M4): call zeroconf.Register with TXTRecord() and wrap the returned
// *zeroconf.Server in an Advertiser.
func Advertise(ctx context.Context, logger *slog.Logger, self protocol.SelfDevice, port int) (Advertiser, error) {
	_ = ctx
	_ = self
	_ = port
	if logger != nil {
		logger.Debug("mDNS advertise requested but not implemented", "serviceType", ServiceType, "domain", Domain)
	}
	return nil, ErrNotImplemented
}

// Browse watches the local network for peers and emits every observed device on
// out until ctx is cancelled. It closes out before returning, so a consumer can
// simply range over the channel.
//
// TODO(M4): call zeroconf.Browse and translate each *zeroconf.ServiceEntry into
// a protocol.Device via DeviceFromTXT.
func Browse(ctx context.Context, logger *slog.Logger, out chan<- protocol.Device) error {
	if out != nil {
		defer close(out)
	}
	_ = ctx
	if logger != nil {
		logger.Debug("mDNS browse requested but not implemented", "serviceType", ServiceType, "domain", Domain)
	}
	return ErrNotImplemented
}

// TXTRecord renders a device identity as the DNS-SD TXT key=value slice that
// zeroconf.Register expects.
func TXTRecord(self protocol.SelfDevice) []string {
	return []string{
		TXTKeyID + "=" + self.ID,
		TXTKeyName + "=" + self.Name,
		TXTKeyVersion + "=" + self.Version,
		TXTKeyProtocol + "=" + strconv.Itoa(protocol.ProtocolVersion),
	}
}

// DeviceFromTXT reassembles a peer from a browse result: the TXT key=value
// pairs plus the address and port taken from the A/AAAA and SRV records.
//
// TODO(M4): call from Browse once zeroconf entries are flowing.
func DeviceFromTXT(txt []string, addr net.IP, port int, lastSeen int64) protocol.Device {
	fields := make(map[string]string, len(txt))
	for _, kv := range txt {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				fields[kv[:i]] = kv[i+1:]
				break
			}
		}
	}

	address := ""
	if addr != nil {
		address = addr.String()
	}

	return protocol.Device{
		ID:       fields[TXTKeyID],
		Name:     fields[TXTKeyName],
		Address:  address,
		Port:     port,
		Platform: protocol.PlatformUnknown, // not advertised in TXT; resolved via GET /info
		Version:  fields[TXTKeyVersion],
		Status:   protocol.StatusOnline,
		LastSeen: lastSeen,
	}
}
