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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadConfig(tc.args, env(tc.env), io.Discard); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
