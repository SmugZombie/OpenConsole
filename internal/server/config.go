// Package server implements the OpenConsole relay's HTTP API.
//
// Phase 1 exposes only session bookkeeping. The tunnel endpoints that will
// carry terminal traffic are intentionally absent; when they arrive they will
// sit behind the same Config and Server, and will talk to the same
// session.Manager.
package server

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Defaults for every configurable value.
const (
	DefaultListenAddr = ":8080"
	DefaultSessionTTL = 30 * time.Minute
	DefaultLogLevel   = "info"

	// Timeouts guard against slow-loris style clients holding connections
	// open. They are generous enough for ordinary API calls and will need
	// per-route relaxation once long-lived WebSocket tunnels exist.
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 15 * time.Second
	DefaultWriteTimeout      = 15 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
	DefaultShutdownTimeout   = 10 * time.Second
)

// Config holds the relay's runtime configuration.
type Config struct {
	ListenAddr      string
	SessionTTL      time.Duration
	LogLevel        string
	ShutdownTimeout time.Duration

	// RunHealthCheck makes the process probe a running relay and exit,
	// instead of serving. It backs the container HEALTHCHECK.
	RunHealthCheck bool
}

// DefaultConfig returns the configuration used when nothing is overridden.
func DefaultConfig() Config {
	return Config{
		ListenAddr:      DefaultListenAddr,
		SessionTTL:      DefaultSessionTTL,
		LogLevel:        DefaultLogLevel,
		ShutdownTimeout: DefaultShutdownTimeout,
	}
}

// Environment variables recognised by LoadConfig.
const (
	EnvListenAddr = "OPENCONSOLE_LISTEN_ADDR"
	EnvSessionTTL = "OPENCONSOLE_SESSION_TTL"
	EnvLogLevel   = "OPENCONSOLE_LOG_LEVEL"
)

// LoadConfig resolves configuration from defaults, then environment variables,
// then command-line flags — later sources win. Flags beat the environment so an
// operator can override a systemd/Docker environment for a one-off run.
//
// args excludes the program name. output receives flag usage and errors.
func LoadConfig(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	cfg := DefaultConfig()

	if v := getenv(EnvListenAddr); v != "" {
		cfg.ListenAddr = v
	}
	if v := getenv(EnvSessionTTL); v != "" {
		d, err := parseTTL(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvSessionTTL, err)
		}
		cfg.SessionTTL = d
	}
	if v := getenv(EnvLogLevel); v != "" {
		cfg.LogLevel = v
	}

	fs := flag.NewFlagSet("openconsole-server", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "address to listen on (env "+EnvListenAddr+")")
	ttl := fs.String("session-ttl", cfg.SessionTTL.String(), "session lifetime, e.g. 30m (env "+EnvSessionTTL+")")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level: debug, info, warn, error (env "+EnvLogLevel+")")
	fs.BoolVar(&cfg.RunHealthCheck, "healthcheck", false, "probe a running relay's /health and exit (for container HEALTHCHECK)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	d, err := parseTTL(*ttl)
	if err != nil {
		return Config{}, fmt.Errorf("-session-ttl: %w", err)
	}
	cfg.SessionTTL = d

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that the configuration is usable.
func (c Config) Validate() error {
	if c.ListenAddr == "" {
		return fmt.Errorf("listen address must not be empty")
	}
	if c.SessionTTL <= 0 {
		return fmt.Errorf("session TTL must be positive, got %s", c.SessionTTL)
	}
	if _, err := ParseLogLevel(c.LogLevel); err != nil {
		return err
	}
	return nil
}

// parseTTL accepts a Go duration ("30m", "1h30m"). A bare integer is rejected
// rather than guessed at, so nobody has to wonder whether "30" means seconds.
func parseTTL(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 30m or 1h)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive, got %q", s)
	}
	return d, nil
}

// ParseLogLevel maps a level name to a slog level.
func ParseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
	}
}

// NewLogger builds the structured logger used across the process. Output is
// line-delimited JSON so it drops straight into any log pipeline; there is no
// human-friendly mode yet because the server is expected to run as a service.
func NewLogger(w io.Writer, level string) (*slog.Logger, error) {
	lvl, err := ParseLogLevel(level)
	if err != nil {
		return nil, err
	}
	if w == nil {
		w = os.Stderr
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}
