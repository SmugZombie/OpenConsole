package client

import (
	"io"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// Someone who has just installed the client has no relay of their own, so the
// default has to be one that works.
func TestDefaultServerIsThePublicRelay(t *testing.T) {
	if DefaultServer != "https://openconsole.dev" {
		t.Fatalf("DefaultServer = %q", DefaultServer)
	}
	// https, not http: the relay is reached over the internet, and a default
	// that quietly spoke plaintext to a public host would be the wrong thing
	// to make easy.
	if !strings.HasPrefix(DefaultServer, "https://") {
		t.Fatalf("DefaultServer = %q, want https", DefaultServer)
	}
	cfg, err := LoadConfig(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != DefaultServer {
		t.Fatalf("Server = %q, want %q", cfg.Server, DefaultServer)
	}
}

func TestLocalFlag(t *testing.T) {
	cfg, err := LoadConfig([]string{"-local"}, env(nil), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != LocalServer {
		t.Fatalf("Server = %q, want %q", cfg.Server, LocalServer)
	}

	// An explicit -server is something the user typed; -local must not
	// silently discard it.
	cfg, err = LoadConfig([]string{"-local", "-server", "https://relay.example.com"}, env(nil), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "https://relay.example.com" {
		t.Fatalf("-server was overridden by -local: %q", cfg.Server)
	}

	// The environment is weaker than a flag, so -local wins over it.
	cfg, err = LoadConfig([]string{"-local"},
		env(map[string]string{EnvServer: "https://relay.example.com"}), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != LocalServer {
		t.Fatalf("Server = %q, want %q", cfg.Server, LocalServer)
	}
}

func TestLoadConfigPrecedence(t *testing.T) {
	cfg, err := LoadConfig(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != DefaultServer {
		t.Fatalf("Server = %q, want %q", cfg.Server, DefaultServer)
	}

	cfg, err = LoadConfig(nil, env(map[string]string{EnvServer: "https://relay.example.com"}), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "https://relay.example.com" {
		t.Fatalf("env not applied: %q", cfg.Server)
	}

	cfg, err = LoadConfig([]string{"-server", "http://localhost:9999"},
		env(map[string]string{EnvServer: "https://relay.example.com"}), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server != "http://localhost:9999" {
		t.Fatalf("flag did not win: %q", cfg.Server)
	}
}

func TestLoadConfigRejectsBadServer(t *testing.T) {
	for _, arg := range []string{"", "relay.example.com", "ftp://relay.example.com", "://nope"} {
		if _, err := LoadConfig([]string{"-server", arg}, env(nil), io.Discard); err == nil {
			t.Fatalf("expected an error for -server %q", arg)
		}
	}
}
