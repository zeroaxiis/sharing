// Command daemon runs the Sharing local daemon.
//
// It owns two listeners, and the split is the whole security design:
//
//	127.0.0.1:8765  CONTROL  the browser extension driving its own daemon
//	0.0.0.0:8766    PEER     other daemons on the LAN, pairing and signalling
//
// The control listener never leaves loopback, so nothing on the network can
// enumerate this machine or start a transfer. The peer listener is reachable
// from the LAN and therefore refuses everything except a pairing request until
// a human on both ends approves one.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/zeroaxiis/sharing/daemon/config"
	"github.com/zeroaxiis/sharing/daemon/discovery"
	"github.com/zeroaxiis/sharing/daemon/peer"
	"github.com/zeroaxiis/sharing/daemon/protocol"
	"github.com/zeroaxiis/sharing/daemon/server"
)

// discoveryQueue is how many mDNS events may queue between the browser and the
// roster. Events are cheap to apply, so this only absorbs a burst; a full
// channel would block the browse loop rather than lose a device.
const discoveryQueue = 64

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if setup failed early, so report to
		// stderr directly and exit non-zero for the supervising process.
		fmt.Fprintf(os.Stderr, "sharing daemon: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port     = flag.Int("port", server.DefaultPort, "loopback TCP port for the control listener")
		peerPort = flag.Int("peer-port", protocol.DefaultPeerPort, "LAN TCP port for the peer listener")
		name     = flag.String("name", "", "device name shown to peers (default: this machine hostname)")
		verbose  = flag.Bool("verbose", false, "enable debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if *peerPort < 1 || *peerPort > 65535 {
		return fmt.Errorf("peer port %d out of range", *peerPort)
	}
	if *peerPort == *port {
		return fmt.Errorf("peer port %d must differ from the control port", *peerPort)
	}

	cfg, err := config.Load(logger, *name)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	trust, err := config.LoadTrustStore(logger)
	if err != nil {
		return fmt.Errorf("load trust store: %w", err)
	}
	logger.Info("trust store loaded", "path", trust.Path(), "pairedDevices", trust.Len())

	// The two layers reference each other: the peer layer reports up through a
	// peer.Handler, and the control server dials down through a PeerRouter.
	// handlerRef holds the upward half so both can be constructed, then wired
	// before either starts serving.
	handlerRef := &peerHandler{}

	peers, err := peer.New(peer.Options{
		Self:    cfg.SelfDevice(),
		Port:    *peerPort,
		Trust:   trust,
		Handler: handlerRef,
		Logger:  logger,
	})
	if err != nil {
		return fmt.Errorf("create peer listener: %w", err)
	}

	srv, err := server.New(server.Options{
		Config: cfg,
		Port:   *port,
		Logger: logger,
		Trust:  trust,
		Peers:  peers,
	})
	if err != nil {
		return fmt.Errorf("create control server: %w", err)
	}
	handlerRef.attach(srv)

	// Bind both sockets before announcing anything, so a port conflict is
	// reported instead of a banner advertising a URL nothing is listening on.
	// The peer port is the one likely to collide, and failing here rather than
	// half-started keeps "the daemon is up" from meaning "but invisible".
	if err := peers.Listen(); err != nil {
		return fmt.Errorf("bind peer listener: %w", err)
	}
	if err := srv.Listen(); err != nil {
		return err
	}

	// SIGINT/SIGTERM cancel the root context, which unwinds both listeners, the
	// mDNS registration, the browse loop and every per-client goroutine.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Advertising is not load-bearing for the control plane: if mDNS cannot
	// bind, the extension still works and paired devices can still be dialled
	// by address. Warn loudly and carry on rather than refusing to start.
	advertiser, err := discovery.Advertise(ctx, logger, cfg.SelfDevice(), *peerPort)
	switch {
	case err != nil:
		logger.Warn("mDNS advertisement failed; this device will not be discoverable",
			"error", err, "hint", "allow the Windows Defender prompt for Private networks, or check that another mDNS responder is not holding UDP 5353")
	case advertiser != nil:
		defer func() {
			if closeErr := advertiser.Close(); closeErr != nil {
				logger.Warn("withdraw mDNS advertisement", "error", closeErr)
			}
		}()
	}

	printBanner(cfg, srv, peers, *peerPort)

	var (
		wg   sync.WaitGroup
		errs = make(chan error, 2)
	)

	// The control server and the peer listener are both fatal: if either stops
	// unexpectedly the daemon is only half alive, which is worse than down.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if runErr := srv.Run(ctx); runErr != nil {
			errs <- fmt.Errorf("control server: %w", runErr)
			cancel()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if runErr := peers.Run(ctx); runErr != nil {
			errs <- fmt.Errorf("peer listener: %w", runErr)
			cancel()
		}
	}()

	events := make(chan discovery.Event, discoveryQueue)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if browseErr := discovery.Browse(ctx, logger, cfg.DeviceID, events); browseErr != nil && ctx.Err() == nil {
			// Not fatal, but the user needs to know why the device list is
			// empty: a silent firewall deny looks exactly like an empty LAN.
			logger.Warn("mDNS browse stopped; no devices will be discovered",
				"error", browseErr, "hint", "allow the Windows Defender prompt for Private networks")
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		consumeDiscovery(ctx, logger, srv, events)
	}()

	wg.Wait()
	close(errs)

	var runErr error
	for e := range errs {
		runErr = errors.Join(runErr, e)
	}
	if runErr != nil {
		return runErr
	}

	logger.Info("daemon stopped")
	return nil
}

// consumeDiscovery folds mDNS events into the control server device roster.
//
// It selects on ctx as well as the channel: Browse closes the channel on a
// clean exit, but a hard failure must not be able to leave this goroutine
// parked forever and hold shutdown open.
func consumeDiscovery(ctx context.Context, logger *slog.Logger, srv *server.Server, events <-chan discovery.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			switch ev.Kind {
			case discovery.EventAdd, discovery.EventUpdate:
				srv.UpsertDevice(ev.Device)
			case discovery.EventRemove:
				srv.RemoveDevice(ev.Device.ID)
			default:
				logger.Warn("ignoring unknown discovery event", "kind", string(ev.Kind), "deviceId", ev.Device.ID)
			}
		}
	}
}

// peerHandler forwards peer-layer callbacks to the control server.
//
// It exists to break a construction cycle: peer.New needs a Handler, and the
// only Handler is the control server, which needs the peer layer. The pointer
// is stored once during start-up and read atomically, because the peer layer
// calls in from its own goroutines.
type peerHandler struct {
	srv atomic.Pointer[server.Server]
}

func (h *peerHandler) attach(srv *server.Server) { h.srv.Store(srv) }

func (h *peerHandler) OnPairCode(deviceID, name, code, direction string) {
	if srv := h.srv.Load(); srv != nil {
		srv.OnPairCode(deviceID, name, code, direction)
	}
}

func (h *peerHandler) OnPairResult(deviceID string, paired bool, reason string) {
	if srv := h.srv.Load(); srv != nil {
		srv.OnPairResult(deviceID, paired, reason)
	}
}

func (h *peerHandler) OnPeerState(deviceID, state, message string) {
	if srv := h.srv.Load(); srv != nil {
		srv.OnPeerState(deviceID, state, message)
	}
}

func (h *peerHandler) OnSignal(msg protocol.PeerSignalMessage) {
	if srv := h.srv.Load(); srv != nil {
		srv.OnSignal(msg)
	}
}

func printBanner(cfg *config.Config, srv *server.Server, peers *peer.Manager, peerPort int) {
	configPath, err := config.Path()
	if err != nil {
		configPath = "(unknown)"
	}
	trustPath, err := config.TrustPath()
	if err != nil {
		trustPath = "(unknown)"
	}

	fmt.Fprintf(os.Stdout, ""+
		"Sharing daemon %s (protocol v%d)\n"+
		"  device id : %s\n"+
		"  name      : %s\n"+
		"  platform  : %s\n"+
		"  http      : %s\n"+
		"  websocket : %s\n"+
		"  peer      : ws://%s%s\n"+
		"  mdns      : %s.%s.%s (port %d)\n"+
		"  config    : %s\n"+
		"  trust     : %s\n",
		config.Version,
		protocol.ProtocolVersion,
		cfg.DeviceID,
		cfg.Name,
		config.Platform(),
		srv.URL(),
		srv.WebSocketURL(),
		peers.Addr(),
		protocol.PeerWebSocketPath,
		mdnsInstanceName(cfg),
		protocol.ServiceType,
		protocol.Domain,
		peerPort,
		configPath,
		trustPath,
	)
}

// mdnsInstanceName mirrors the DNS-SD instance name the discovery package
// registers: a sanitised device name capped at 40 characters, a dash, and the
// first eight characters of the device id.
//
// It is display only - it is printed so the user can match what a Bonjour
// browser shows against this daemon - but it deliberately reproduces
// discovery's rule rather than approximating it, because a banner that
// disagrees with what is actually on the wire is worse than no banner. The
// rule lives in discovery.instanceName, which is unexported; if it ever moves,
// this must follow.
const mdnsNameMaxLen = 40

func mdnsInstanceName(cfg *config.Config) string {
	var b strings.Builder
	prevDash := false
	for _, r := range cfg.Name {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	label := strings.Trim(b.String(), "-")
	if len(label) > mdnsNameMaxLen {
		label = strings.Trim(label[:mdnsNameMaxLen], "-")
	}
	if label == "" {
		label = "sharing"
	}

	suffix := cfg.DeviceID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if suffix == "" {
		return label
	}
	return label + "-" + suffix
}
