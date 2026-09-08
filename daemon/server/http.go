// Package server exposes the daemon local control surface: a small JSON HTTP
// API and a WebSocket endpoint, both bound to loopback only.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeroaxiis/sharing/daemon/config"
	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// BindHost is the only interface the daemon listens on.
//
// This is deliberately 127.0.0.1 and never 0.0.0.0: binding the wildcard
// address would publish an unauthenticated control API to every machine on the
// LAN (and, behind a misconfigured router, to the internet), letting anyone on
// the coffee-shop Wi-Fi enumerate this device and drive its transfers. Peer to
// peer traffic goes over the separately authenticated WebRTC path instead, so
// the control plane has no reason to leave the local host.
const BindHost = "127.0.0.1"

// DefaultPort is the daemon default listen port.
const DefaultPort = 8765

// WebSocketPath is the WebSocket upgrade route.
const WebSocketPath = "/ws"

// Extension origin schemes accepted by the CORS and WebSocket origin checks.
const (
	chromeExtensionScheme  = "chrome-extension://"
	firefoxExtensionScheme = "moz-extension://"
	safariExtensionScheme  = "safari-web-extension://"
)

// Options configures a Server.
type Options struct {
	// Config is the resolved device identity. Required.
	Config *config.Config
	// Port is the loopback TCP port to listen on. Zero selects DefaultPort.
	Port int
	// Logger receives structured logs. Nil uses slog.Default.
	Logger *slog.Logger
}

// Server owns the HTTP listener, the routing table and the WebSocket hub.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	port    int
	http    *http.Server
	hub     *hub
	started time.Time

	mu sync.Mutex
	ln net.Listener
}

// New builds a Server. It does not bind a socket; call Run for that.
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("server: Options.Config is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	port := opts.Port
	if port == 0 {
		port = DefaultPort
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("server: port %d out of range", port)
	}

	s := &Server{
		cfg:     opts.Config,
		log:     logger,
		port:    port,
		hub:     newHub(logger),
		started: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/info", s.handleInfo)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc(WebSocketPath, s.handleWebSocket)
	mux.HandleFunc("/", s.handleNotFound)

	s.http = &http.Server{
		Addr:    s.Addr(),
		Handler: s.withCORS(mux),
		// The WebSocket handler hijacks the connection, so a global write
		// timeout would kill long-lived sockets. Bound only the handshake.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}

	return s, nil
}

// Addr is the host:port the server listens on.
func (s *Server) Addr() string {
	return net.JoinHostPort(BindHost, strconv.Itoa(s.port))
}

// URL is the base HTTP URL clients should use.
func (s *Server) URL() string {
	return "http://" + s.Addr()
}

// WebSocketURL is the ws:// URL clients should connect to.
func (s *Server) WebSocketURL() string {
	return "ws://" + s.Addr() + WebSocketPath
}

// Listen binds the loopback socket. It is separate from Run so a caller can
// fail fast on a port conflict before announcing a listen URL that does not
// exist. Run calls it automatically if it has not been called already.
func (s *Server) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.Addr())
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.Addr(), err)
	}
	s.ln = ln
	return nil
}

// Run serves until ctx is cancelled, then drains in-flight requests and closes
// every WebSocket client. It returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()

	// Give every WebSocket connection a context that dies with the server.
	wsCtx, cancelWS := context.WithCancel(ctx)
	defer cancelWS()
	s.hub.setBaseContext(wsCtx)

	serveErr := make(chan error, 1)
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("serve http: %w", err)
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down http server")
	cancelWS()
	s.hub.closeAll()

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// Fall back to a hard close so a wedged client cannot hold the process open.
		if closeErr := s.http.Close(); closeErr != nil {
			return fmt.Errorf("graceful shutdown: %w (forced close also failed: %v)", err, closeErr)
		}
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	if err := <-serveErr; err != nil {
		return err
	}
	return nil
}

// -----------------------------------------------------------------------------
// Handlers
// -----------------------------------------------------------------------------

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, protocol.HTTPError{Error: "method not allowed"}, s.log)
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.DaemonInfo(), s.log)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, protocol.HTTPError{Error: "method not allowed"}, s.log)
		return
	}
	writeJSON(w, http.StatusOK, protocol.HealthResponse{
		Status:        "ok",
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
	}, s.log)
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.log.Debug("unrouted request", "method", r.Method, "path", r.URL.Path)
	writeJSON(w, http.StatusNotFound, protocol.HTTPError{Error: "not found"}, s.log)
}

// -----------------------------------------------------------------------------
// CORS / Private Network Access
// -----------------------------------------------------------------------------

// withCORS attaches the CORS and Private Network Access headers to every
// response, including 404s and the preflight, and answers OPTIONS with 204.
//
// Chrome requires Access-Control-Allow-Private-Network on the preflight before
// a public-origin page may talk to 127.0.0.1; without it the extension fetch is
// blocked before the daemon ever sees the real request.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// Responses vary by Origin even when the header is omitted, so caches
		// must never reuse an allowed response for a disallowed origin.
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Allow-Private-Network", "true")
		h.Set("Access-Control-Max-Age", "600")

		origin := r.Header.Get("Origin")
		switch {
		case origin == "":
			// No Origin at all: curl, a native client, or a same-process probe.
			// Allowed, but deliberately gets no Allow-Origin header - there is
			// no browser to satisfy and echoing "*" would widen the surface.
		case OriginAllowed(origin):
			h.Set("Access-Control-Allow-Origin", origin)
		default:
			// Omit the header entirely. The browser then blocks the response on
			// the client side, which is the behaviour the spec asks for.
			s.log.Debug("rejected cross-origin request", "origin", origin, "path", r.URL.Path)
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// OriginAllowed reports whether an Origin header value may talk to the daemon.
//
// Allowed: any browser extension origin (chrome-extension://, moz-extension://,
// safari-web-extension://) and a loopback HTTP dev server on any port
// (http://localhost:PORT, http://127.0.0.1:PORT). Everything else is rejected,
// which keeps an arbitrary web page from scripting the daemon behind the user
// back.
func OriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}

	lower := strings.ToLower(origin)
	for _, scheme := range []string{chromeExtensionScheme, firefoxExtensionScheme, safariExtensionScheme} {
		if strings.HasPrefix(lower, scheme) {
			// Require a non-empty extension id after the scheme.
			return len(lower) > len(scheme)
		}
	}

	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	// An Origin is scheme://host[:port] only; anything else means it was forged.
	if u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host, port := u.Hostname(), u.Port()
	if host != "localhost" && host != "127.0.0.1" {
		return false
	}
	if port == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

func writeJSON(w http.ResponseWriter, status int, payload any, logger *slog.Logger) {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.Error("encode response body", "error", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		logger.Debug("write response body", "error", err)
	}
}
