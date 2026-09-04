// Command openconsole-server is the OpenConsole relay.
//
// It brokers sessions between a host CLI and joining guests. Phase 1 serves
// the session API only; the tunnel endpoints that carry terminal traffic are
// added in a later phase.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/SmugZombie/OpenConsole/internal/server"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/openconsole-server
var version = "dev"

func main() {
	if err := run(); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "openconsole-server: %v\n", err)
		}
		os.Exit(1)
	}
}

func run() error {
	cfg, err := server.LoadConfig(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		return err
	}

	// The health check runs as the same binary against an already-running
	// relay, so the container image needs no shell or HTTP client of its own.
	if cfg.RunHealthCheck {
		return server.HealthCheck(context.Background(), cfg.ListenAddr)
	}

	log, err := server.NewLogger(os.Stderr, cfg.LogLevel)
	if err != nil {
		return err
	}

	// Cancelling on SIGINT/SIGTERM is what drives graceful shutdown; a second
	// signal restores the default behaviour so an operator can always force
	// the process to die.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(cfg, log, version)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
