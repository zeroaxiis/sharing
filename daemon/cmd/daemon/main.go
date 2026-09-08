// Command daemon runs the Nearby Share local daemon: a loopback-only HTTP and
// WebSocket control surface that browser extensions and native clients drive.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zeroaxiis/sharing/daemon/config"
	"github.com/zeroaxiis/sharing/daemon/protocol"
	"github.com/zeroaxiis/sharing/daemon/server"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if setup failed early, so report to
		// stderr directly and exit non-zero for the supervising process.
		fmt.Fprintf(os.Stderr, "nearby-share daemon: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port    = flag.Int("port", server.DefaultPort, "loopback TCP port to listen on")
		name    = flag.String("name", "", "device name shown to peers (default: this machine hostname)")
		verbose = flag.Bool("verbose", false, "enable debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg, err := config.Load(logger, *name)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	srv, err := server.New(server.Options{
		Config: cfg,
		Port:   *port,
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	// Bind before announcing anything, so a port conflict is reported instead of
	// a banner advertising a URL nothing is listening on.
	if err := srv.Listen(); err != nil {
		return err
	}

	// SIGINT/SIGTERM cancel the root context, which unwinds the HTTP server,
	// the WebSocket hub and every per-client goroutine in that order.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	printBanner(cfg, srv)

	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("run server: %w", err)
	}

	logger.Info("daemon stopped")
	return nil
}

func printBanner(cfg *config.Config, srv *server.Server) {
	configPath, err := config.Path()
	if err != nil {
		configPath = "(unknown)"
	}

	fmt.Fprintf(os.Stdout, ""+
		"Nearby Share daemon %s (protocol v%d)\n"+
		"  device id : %s\n"+
		"  name      : %s\n"+
		"  platform  : %s\n"+
		"  http      : %s\n"+
		"  websocket : %s\n"+
		"  config    : %s\n",
		config.Version,
		protocol.ProtocolVersion,
		cfg.DeviceID,
		cfg.Name,
		config.Platform(),
		srv.URL(),
		srv.WebSocketURL(),
		configPath,
	)
}
