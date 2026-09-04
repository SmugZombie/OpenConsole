package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// hostKeyFileMode is deliberately owner-only. A readable host key lets anyone
// on the machine impersonate the relay to every guest that ever trusted it.
const hostKeyFileMode fs.FileMode = 0o600

// LoadOrCreateHostKey returns the relay's SSH host identity.
//
// If path names an existing key it is loaded; if it does not exist, a new
// ed25519 key is generated and written there. Ed25519 rather than RSA: smaller,
// faster, no key-size decision to get wrong, and supported by every SSH client
// that has shipped in the last decade.
//
// The host key must be stable. SSH clients pin it on first connection, so a key
// that changes on restart greets every returning guest with the loud warning
// that normally means an active attack — and trains them to ignore it.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("sshd: parsing host key %s: %w", path, err)
		}
		return signer, nil
	case errors.Is(err, fs.ErrNotExist):
		return createHostKey(path)
	default:
		return nil, fmt.Errorf("sshd: reading host key %s: %w", path, err)
	}
}

// GenerateHostKey returns a new in-memory host key.
//
// Used when no path is configured. The caller is expected to warn: an ephemeral
// key changes on every restart, which is fine for a local trial and wrong for
// anything a person connects to twice.
func GenerateHostKey() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sshd: generating host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("sshd: building host key signer: %w", err)
	}
	return signer, nil
}

// createHostKey generates a key and persists it at path.
func createHostKey(path string) (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sshd: generating host key: %w", err)
	}

	// OpenSSH's own private key format, rather than PKCS#8, so an operator can
	// inspect and rotate the key with the tools they already have:
	// `ssh-keygen -l -f host_key` works on this and does not on PKCS#8.
	block, err := ssh.MarshalPrivateKey(priv, "openconsole relay host key")
	if err != nil {
		return nil, fmt.Errorf("sshd: encoding host key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(block)

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("sshd: creating %s: %w", dir, err)
		}
	}

	// O_EXCL so a key created by another process in the meantime is never
	// silently overwritten — that would invalidate every client's known_hosts
	// entry without anyone noticing.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, hostKeyFileMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return LoadOrCreateHostKey(path)
		}
		return nil, fmt.Errorf("sshd: creating host key %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Write(pemBytes); err != nil {
		return nil, fmt.Errorf("sshd: writing host key %s: %w", path, err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("sshd: building host key signer: %w", err)
	}
	return signer, nil
}

// Fingerprint returns the SHA256 fingerprint of a signer's public key, in the
// form OpenSSH prints. Operators publish this so guests can verify the relay on
// first connection.
func Fingerprint(signer ssh.Signer) string {
	return ssh.FingerprintSHA256(signer.PublicKey())
}
