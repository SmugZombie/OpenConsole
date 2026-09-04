package sshd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateHostKeyCreatesAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "host_key")

	first, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateHostKey: %v", err)
	}

	// The key must be stable across restarts: SSH clients pin it, and a key
	// that changes greets every returning guest with a warning that normally
	// means an active attack.
	second, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateHostKey (reload): %v", err)
	}
	if Fingerprint(first) != Fingerprint(second) {
		t.Fatal("host key changed between loads")
	}
}

func TestCreatedHostKeyIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_key")
	if _, err := LoadOrCreateHostKey(path); err != nil {
		t.Fatalf("LoadOrCreateHostKey: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// A readable host key lets anyone on the box impersonate the relay.
	if perm := info.Mode().Perm(); perm != hostKeyFileMode {
		t.Fatalf("host key mode = %v, want %v", perm, hostKeyFileMode)
	}
}

func TestGenerateHostKeyIsEphemeralAndUnique(t *testing.T) {
	a, err := GenerateHostKey()
	if err != nil {
		t.Fatalf("GenerateHostKey: %v", err)
	}
	b, err := GenerateHostKey()
	if err != nil {
		t.Fatalf("GenerateHostKey: %v", err)
	}
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("two generated host keys are identical")
	}
	if got := a.PublicKey().Type(); got != "ssh-ed25519" {
		t.Fatalf("key type = %q, want ssh-ed25519", got)
	}
}

func TestLoadHostKeyRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_key")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Silently replacing an unreadable key would invalidate every client's
	// known_hosts entry, so this must fail loudly instead.
	if _, err := LoadOrCreateHostKey(path); err == nil {
		t.Fatal("expected an error for a malformed key file")
	}
}

func TestFingerprintFormat(t *testing.T) {
	k, err := GenerateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	// Operators publish this, so it must be what OpenSSH prints.
	if fp := Fingerprint(k); !strings.HasPrefix(fp, "SHA256:") {
		t.Fatalf("fingerprint = %q, want a SHA256: prefix", fp)
	}
}
