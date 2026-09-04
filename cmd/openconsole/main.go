// Command openconsole shares a local terminal through an OpenConsole relay, and
// joins terminals shared by others.
//
//	openconsole                        share this terminal
//	openconsole join <ticket>          join a shared terminal
//	openconsole version                print version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/SmugZombie/OpenConsole/internal/client"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/openconsole
var version = "dev"

// EnvTicket supplies a join ticket without putting it on the command line,
// where `ps` would expose it to every user on the machine.
const EnvTicket = "OPENCONSOLE_TICKET"

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "openconsole: %v\n", err)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// run dispatches a subcommand and returns the process exit code.
//
// When sharing, the exit code is the shell's own, so `openconsole` is
// transparent in a script the way `ssh` is.
func run(args []string) (int, error) {
	// Ctrl-C is not handled here: the local terminal is in raw mode while
	// sharing, so an interrupt goes to the shared shell where the user
	// expects it. This context exists for SIGTERM and SIGHUP.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	switch {
	case len(args) > 0 && args[0] == "join":
		return runJoin(ctx, args[1:])
	case len(args) > 0 && args[0] == "version":
		fmt.Printf("openconsole %s\n", version)
		return 0, nil
	default:
		return runShare(ctx, args)
	}
}

func runShare(ctx context.Context, args []string) (int, error) {
	cfg, err := client.LoadConfig(args, os.Getenv, os.Stderr)
	if err != nil {
		return 1, err
	}
	if cfg.ShowVersion {
		fmt.Printf("openconsole %s\n", version)
		return 0, nil
	}
	return client.Share(ctx, cfg, os.Stdin, os.Stdout, os.Stderr)
}

func runJoin(ctx context.Context, args []string) (int, error) {
	// The ticket is a positional argument, so pull it out before flag parsing.
	ticket := os.Getenv(EnvTicket)
	rest := args
	if len(args) > 0 && !isFlag(args[0]) {
		ticket, rest = args[0], args[1:]
	}

	cfg, err := client.LoadConfig(rest, os.Getenv, os.Stderr)
	if err != nil {
		return 1, err
	}
	if ticket == "" {
		return 1, fmt.Errorf("usage: openconsole join <ticket>  (or set %s)", EnvTicket)
	}
	if err := client.Join(ctx, cfg, ticket, os.Stdin, os.Stdout, os.Stderr); err != nil {
		return 1, err
	}
	return 0, nil
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }
