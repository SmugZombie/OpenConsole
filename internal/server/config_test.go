package server

import (
	"io"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.SessionTTL != DefaultSessionTTL {
		t.Fatalf("SessionTTL = %s, want %s", cfg.SessionTTL, DefaultSessionTTL)
	}
	if cfg.LogLevel != DefaultLogLevel {
		t.Fatalf("LogLevel = %q, want %q", cfg.LogLevel, DefaultLogLevel)
	}
}

func TestLoadConfigEnv(t *testing.T) {
	cfg, err := LoadConfig(nil, env(map[string]string{
		EnvListenAddr: "127.0.0.1:9000",
		EnvSessionTTL: "5m",
		EnvLogLevel:   "debug",
	}), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" || cfg.SessionTTL != 5*time.Minute || cfg.LogLevel != "debug" {
		t.Fatalf("env not applied: %+v", cfg)
	}
}

func TestLoadConfigFlagsBeatEnv(t *testing.T) {
	cfg, err := LoadConfig(
		[]string{"-listen", ":1234", "-session-ttl", "90s", "-log-level", "warn"},
		env(map[string]string{
			EnvListenAddr: "127.0.0.1:9000",
			EnvSessionTTL: "5m",
			EnvLogLevel:   "debug",
		}),
		io.Discard,
	)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ListenAddr != ":1234" || cfg.SessionTTL != 90*time.Second || cfg.LogLevel != "warn" {
		t.Fatalf("flags did not win: %+v", cfg)
	}
}

func TestLoadConfigSSHIsOffByDefault(t *testing.T) {
	// An upgrade must never start listening on a new port unasked.
	cfg, err := LoadConfig(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SSHEnabled() {
		t.Fatalf("SSH is enabled by default (addr %q)", cfg.SSHAddr)
	}
}

func TestLoadConfigSSH(t *testing.T) {
	cfg, err := LoadConfig(nil, env(map[string]string{
		EnvSSHAddr:    ":2222",
		EnvSSHHostKey: "/var/lib/openconsole/host_key",
	}), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.SSHEnabled() || cfg.SSHAddr != ":2222" {
		t.Fatalf("SSHAddr = %q", cfg.SSHAddr)
	}
	if cfg.SSHHostKey != "/var/lib/openconsole/host_key" {
		t.Fatalf("SSHHostKey = %q", cfg.SSHHostKey)
	}

	cfg, err = LoadConfig([]string{"-ssh-listen", ":2200"},
		env(map[string]string{EnvSSHAddr: ":2222"}), io.Discard)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SSHAddr != ":2200" {
		t.Fatalf("flag did not win: %q", cfg.SSHAddr)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"bare integer ttl", nil, map[string]string{EnvSessionTTL: "30"}},
		{"negative ttl", []string{"-session-ttl", "-5m"}, nil},
		{"zero ttl", []string{"-session-ttl", "0s"}, nil},
		{"unknown log level", []string{"-log-level", "loud"}, nil},
		{"empty listen", []string{"-listen", ""}, nil},
		// A host key with no listener is a configuration someone thinks is
		// enabling SSH. Say so rather than silently ignoring it.
		{"host key without ssh", []string{"-ssh-host-key", "/tmp/k"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadConfig(tc.args, env(tc.env), io.Discard); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
