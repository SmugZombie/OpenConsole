// Package client holds the host-side CLI logic.
//
// Phase 1 covers configuration only: the CLI can resolve which relay it would
// talk to and report its version. PTY handling, the tunnel and session
// creation arrive in later phases and will build on this Config.
package client

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// DefaultServer is the relay used when none is configured. It points at a
// local development relay because there is no public relay to default to.
const DefaultServer = "http://localhost:8080"

// EnvServer overrides the relay address.
const EnvServer = "OPENCONSOLE_SERVER"

// Config is the CLI's runtime configuration.
type Config struct {
	// Server is the base URL of the relay, e.g. https://console.example.com.
	Server string
	// ShowVersion requests version output instead of starting a session.
	ShowVersion bool
}

// LoadConfig resolves CLI configuration from defaults, the environment and
// flags, in that order of increasing precedence. args excludes the program
// name.
func LoadConfig(args []string, getenv func(string) string, output io.Writer) (Config, error) {
	cfg := Config{Server: DefaultServer}
	if v := getenv(EnvServer); v != "" {
		cfg.Server = v
	}

	fs := flag.NewFlagSet("openconsole", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.StringVar(&cfg.Server, "server", cfg.Server, "relay base URL (env "+EnvServer+")")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(output, "openconsole - share a terminal through an OpenConsole relay\n\n")
		fmt.Fprintf(output, "Usage:\n  openconsole [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(output, "\nTerminal sharing is not implemented yet; see docs/architecture.md.\n")
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that the relay address is a usable absolute HTTP(S) URL.
func (c Config) Validate() error {
	u, err := url.Parse(strings.TrimSpace(c.Server))
	if err != nil {
		return fmt.Errorf("invalid -server %q: %w", c.Server, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid -server %q: expected an http:// or https:// URL", c.Server)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid -server %q: missing host", c.Server)
	}
	return nil
}
