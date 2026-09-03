package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/SmugZombie/OpenConsole/internal/session"
)

// Server owns the listener, the HTTP server and the session manager's
// background sweeper.
type Server struct {
	cfg      Config
	log      *slog.Logger
	sessions *session.Manager
	bridges  *session.Bridges
	http     *http.Server
	ln       net.Listener

	// baseCtx bounds every live tunnel. WebSocket connections are hijacked,
	// so http.Server.Shutdown does not wait for them or close them; cancelling
	// this is what actually ends them.
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

// New builds a Server and binds its listener immediately, so a port conflict
// is reported before the caller believes the service is up.
func New(cfg Config, log *slog.Logger, version string) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	sessions := session.NewManager(session.Options{TTL: cfg.SessionTTL})
	bridges := session.NewBridges(log)

	baseCtx, baseCancel := context.WithCancel(context.Background())
	api := NewAPI(sessions, bridges, log, version, baseCtx)

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		baseCancel()
		return nil, fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}

	return &Server{
		cfg:        cfg,
		log:        log,
		sessions:   sessions,
		bridges:    bridges,
		ln:         ln,
		baseCtx:    baseCtx,
		baseCancel: baseCancel,
		http: &http.Server{
			Handler:           api.Routes(),
			ReadHeaderTimeout: DefaultReadHeaderTimeout,
			IdleTimeout:       DefaultIdleTimeout,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
			// ReadTimeout and WriteTimeout are deliberately unset. They are
			// whole-connection deadlines, and a tunnel carrying an idle
			// terminal would trip them and drop a working session. Slow-client
			// protection comes from ReadHeaderTimeout above, the request body
			// cap in the API, and protocol-level PING/PONG on tunnels.
		},
	}, nil
}

// Addr reports the address the server is actually bound to, which is useful
// when the configured address used port 0.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Run serves until ctx is cancelled, then shuts down gracefully, giving
// in-flight requests up to ShutdownTimeout to finish. It returns nil on a
// clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	sweeperCtx, stopSweeper := context.WithCancel(ctx)
	defer stopSweeper()
	go s.sessions.Run(sweeperCtx)

	errCh := make(chan error, 1)
	go func() {
		// Durations are logged as strings on lifecycle lines: slog's JSON
		// handler renders time.Duration as raw nanoseconds, which is fine for
		// the machine-read request latency below but unreadable for a
		// configuration echo.
		s.log.Info("relay listening",
			slog.String("addr", s.Addr()),
			slog.String("session_ttl", s.cfg.SessionTTL.String()),
		)
		errCh <- s.http.Serve(s.ln)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("shutting down", slog.String("timeout", s.shutdownTimeout().String()))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout())
	defer cancel()

	err := s.http.Shutdown(shutdownCtx)

	// Shutdown does not touch hijacked WebSocket connections, so tunnels are
	// closed explicitly: tell peers first, then cancel the context their
	// goroutines are parked on.
	s.bridges.CloseAll("relay shutting down")
	s.baseCancel()
	stopSweeper()
	s.sessions.Close()
	if err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.log.Info("shutdown complete")
	return nil
}

func (s *Server) shutdownTimeout() time.Duration {
	if s.cfg.ShutdownTimeout <= 0 {
		return DefaultShutdownTimeout
	}
	return s.cfg.ShutdownTimeout
}
