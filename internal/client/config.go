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

// DefaultServer is the relay used when none is configured.
//
// The public relay, so `openconsole` works the moment it is installed. Someone
// who has just run the install script has no relay of their own, and asking
// them to stand one up before they can share a terminal would put the whole
// point of the tool behind a deployment.
//
// https, not http: the token travels inside the tunnel and the relay is
// reached over the internet. A default that quietly spoke plaintext to a
// public host would be the wrong thing to make easy.
const DefaultServer = "https://openconsole.dev"

// LocalServer is the conventional address of a relay run on this machine. It
// is not a default, only the value the docs and the -local flag use.
const LocalServer = "http://localhost:8080"

// EnvServer overrides the relay address.
const EnvServer = "OPENCONSOLE_SERVER"

// Config is the CLI's runtime configuration.
type Config struct {
	// Server is the base URL of the relay, e.g. https://console.example.com.
	Server string
	// Shell overrides the program to run. Empty means $SHELL.
	//
	// This is a local flag on purpose. The shell is chosen by the person at
	// the keyboard; taking it from the relay would be remote code execution.
	Shell string
	// RelayToken is the secret a private relay requires to create a session.
	RelayToken string
	// AllowForward is the host's list of targets guests may reach, as
	// host:port entries or the literal "any". Empty means no forwarding, which
	// is the default: a forward reaches whatever this machine can reach.
	AllowForward Allowlist
	// Forwards are the guest's -L requests.
	Forwards []ForwardSpec
	// ReadOnly asks to join without the ability to type, even when the ticket
	// would allow it. Useful for looking over someone's shoulder without the
	// risk of a stray keystroke.
	ReadOnly bool
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
	// Shorthand for the development loop, which is otherwise the one common
	// case that now needs a flag.
	local := fs.Bool("local", false, "use a relay on this machine ("+LocalServer+")")
	fs.StringVar(&cfg.Shell, "shell", cfg.Shell, "shell to run (default $SHELL)")
	// Read from the environment only; a command line is world-readable.
	cfg.RelayToken = getenv(EnvRelayToken)
	allow := fs.String("allow-forward", "",
		"share only: comma-separated host:port targets guests may reach, or \"any\" (default none)")
	var forwards forwardList
	fs.Var(&forwards, "L", "join only: forward a local port, [bind:]port:host:hostport (repeatable)")
	fs.BoolVar(&cfg.ReadOnly, "read-only", false, "join without typing (join only)")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(output, "openconsole - share a terminal through an OpenConsole relay\n\n")
		fmt.Fprintf(output, "Usage:\n")
		fmt.Fprintf(output, "  openconsole [flags]                 share this terminal\n")
		fmt.Fprintf(output, "  openconsole join <ticket> [flags]   join a shared terminal\n")
		fmt.Fprintf(output, "  openconsole version                 print version\n")
		fmt.Fprintf(output, "\nForwarding:\n")
		fmt.Fprintf(output, "  openconsole -allow-forward localhost:5432\n")
		fmt.Fprintf(output, "  openconsole join <ticket> -L 5432:localhost:5432\n")
		fmt.Fprintf(output, "\nBy default this talks to %s.\n", DefaultServer)
		fmt.Fprintf(output, "Use -local for a relay on this machine, or -server for your own.\n")
		fmt.Fprintf(output, "\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if *local {
		// An explicit -server still wins: -local is a convenience, not an
		// override of something the user typed.
		if !serverWasSet(fs) {
			cfg.Server = LocalServer
		}
	}

	list, err := ParseAllowlist(*allow)
	if err != nil {
		return Config{}, err
	}
	cfg.AllowForward = list
	cfg.Forwards = forwards

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// serverWasSet reports whether -server appeared on the command line, so that
// -local does not quietly discard it.
func serverWasSet(fs *flag.FlagSet) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "server" {
			set = true
		}
	})
	return set
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
