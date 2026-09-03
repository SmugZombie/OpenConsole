package client

import (
	"io"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
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
