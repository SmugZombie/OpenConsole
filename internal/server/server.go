package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/SmugZombie/OpenConsole/internal/session"
	"github.com/SmugZombie/OpenConsole/internal/sshd"
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

	// ssh is the optional SSH listener. Nil when SSH is disabled.
	ssh *sshd.Server

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

	sshServer, err := newSSH(cfg, log, sessions, bridges, version)
	if err != nil {
		baseCancel()
		ln.Close()
		return nil, err
	}
	if sshServer != nil {
		// Advertised to clients so the host CLI can print a working ssh
		// command. The port is taken from the bound listener rather than the
		// configured address, so ":0" in a test resolves correctly.
		if _, port, err := net.SplitHostPort(sshServer.Addr()); err == nil {
			if n, err := strconv.Atoi(port); err == nil {
				api.SetSSHPort(n)
			}
		}
	}

	return &Server{
		cfg:        cfg,
		log:        log,
		sessions:   sessions,
		bridges:    bridges,
		ln:         ln,
		ssh:        sshServer,
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

// SSHAddr reports the SSH listener's address, or "" when SSH is disabled.
func (s *Server) SSHAddr() string {
	if s.ssh == nil {
		return ""
	}
	return s.ssh.Addr()
}

// newSSH builds the SSH listener, or returns nil when SSH is off.
func newSSH(cfg Config, log *slog.Logger, sessions *session.Manager, bridges *session.Bridges, version string) (*sshd.Server, error) {
	if !cfg.SSHEnabled() {
		return nil, nil
	}

	var (
		hostKey ssh.Signer
		err     error
	)
	if cfg.SSHHostKey != "" {
		hostKey, err = sshd.LoadOrCreateHostKey(cfg.SSHHostKey)
	} else {
		hostKey, err = sshd.GenerateHostKey()
		if err == nil {
			// Loud, because the symptom is confusing: every returning guest
			// gets the host-key-changed warning that normally means an attack.
			log.Warn("ssh host key is ephemeral and changes on restart; set -ssh-host-key to persist it")
		}
	}
	if err != nil {
		return nil, err
	}

	return sshd.New(sshd.Options{
		Addr:     cfg.SSHAddr,
		HostKey:  hostKey,
		Sessions: sessions,
		Bridges:  bridges,
		Log:      log,
		Version:  version,
	})
}

// Run serves until ctx is cancelled, then shuts down gracefully, giving
// in-flight requests up to ShutdownTimeout to finish. It returns nil on a
// clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	sweeperCtx, stopSweeper := context.WithCancel(ctx)
	defer stopSweeper()
	go s.sessions.Run(sweeperCtx)

	if s.ssh != nil {
		s.log.Info("ssh joins enabled", slog.String("addr", s.SSHAddr()))
		go func() {
			if err := s.ssh.Run(ctx); err != nil {
				s.log.Error("ssh listener stopped", slog.Any("error", err))
			}
		}()
	}

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

	if s.ssh != nil {
		_ = s.ssh.Close()
	}

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
