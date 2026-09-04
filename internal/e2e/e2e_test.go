package e2e

import (
	"bytes"
	"errors"
	"testing"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

func rootKey(t *testing.T) []byte {
	t.Helper()
	k, err := NewRootKey()
	if err != nil {
		t.Fatalf("NewRootKey: %v", err)
	}
	return k
}

func hostSession(t *testing.T, id string, root []byte) *Session {
	t.Helper()
	s, err := FromRootKey(id, root)
	if err != nil {
		t.Fatalf("FromRootKey: %v", err)
	}
	return s
}

func TestRoundTripBothDirections(t *testing.T) {
	root := rootKey(t)
	host := hostSession(t, "session-a", root)
	guest := hostSession(t, "session-a", root)

	// Terminal output is arbitrary binary, so the payload here is too.
	out := []byte{0x00, 0x1b, '[', 'A', 0xff, 0xfe, '\n'}

	sealed, err := host.SealHostToGuest(0, out)
	if err != nil {
		t.Fatalf("SealHostToGuest: %v", err)
	}
	if bytes.Contains(sealed, out) {
		t.Fatal("the plaintext is visible in the sealed frame")
	}
	got, err := guest.OpenHostToGuest(0, sealed)
	if err != nil {
		t.Fatalf("OpenHostToGuest: %v", err)
	}
	if !bytes.Equal(got, out) {
		t.Fatalf("round trip gave %v", got)
	}

	in := []byte("sudo rm -rf /\r")
	sealed, err = guest.SealGuestToHost(0, in)
	if err != nil {
		t.Fatalf("SealGuestToHost: %v", err)
	}
	got, err = host.OpenGuestToHost(0, sealed)
	if err != nil {
		t.Fatalf("OpenGuestToHost: %v", err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("round trip gave %q", got)
	}
}

// The point of the whole package: someone holding everything the relay holds
// still cannot read the terminal.
func TestRelayCannotDecrypt(t *testing.T) {
	root := rootKey(t)
	host := hostSession(t, "session-a", root)

	sealed, err := host.SealHostToGuest(0, []byte("the password is hunter2"))
	if err != nil {
		t.Fatal(err)
	}

	// A relay knows the session ID and every token. None of them is the key,
	// so the best it can do is guess.
	notTheKey := make([]byte, KeySize)
	copy(notTheKey, "session-id+host-token+guest-token")
	impostor, err := FromRootKey("session-a", notTheKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := impostor.OpenHostToGuest(0, sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a wrong key decrypted the frame: %v", err)
	}
}

// A relay must not be able to change a keystroke, or flip a bit in the output.
func TestTamperingIsDetected(t *testing.T) {
	root := rootKey(t)
	s := hostSession(t, "session-a", root)

	sealed, err := s.SealHostToGuest(0, []byte("ls -la"))
	if err != nil {
		t.Fatal(err)
	}

	for i := range sealed {
		altered := append([]byte(nil), sealed...)
		altered[i] ^= 0x01
		if _, err := s.OpenHostToGuest(0, altered); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("a flipped bit at offset %d went undetected", i)
		}
	}

	// Truncation too.
	for _, n := range []int{0, 1, nonceSize, len(sealed) - 1} {
		if _, err := s.OpenHostToGuest(0, sealed[:n]); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("a frame truncated to %d bytes was accepted", n)
		}
	}
}

// Read-only becomes arithmetic rather than a rule the relay applies.
func TestViewerCannotProduceInput(t *testing.T) {
	root := rootKey(t)
	id := "session-a"
	host := hostSession(t, id, root)

	vk, err := ViewerKey(id, root)
	if err != nil {
		t.Fatalf("ViewerKey: %v", err)
	}
	viewer, err := FromViewerKey(vk)
	if err != nil {
		t.Fatalf("FromViewerKey: %v", err)
	}

	// It can watch.
	sealed, err := host.SealHostToGuest(0, []byte("terminal output"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := viewer.OpenHostToGuest(0, sealed)
	if err != nil {
		t.Fatalf("a viewer could not read the terminal: %v", err)
	}
	if string(got) != "terminal output" {
		t.Fatalf("viewer read %q", got)
	}

	// It cannot type, even with a compliant relay forwarding whatever it sends.
	if _, err := viewer.SealGuestToHost(0, []byte("rm -rf /")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a viewer sealed input: %v", err)
	}
	if viewer.CanWrite() {
		t.Fatal("a viewer reports that it can write")
	}
	if !host.CanWrite() {
		t.Fatal("a host reports that it cannot write")
	}
}

// The viewer key must not be enough to reconstruct the root, or read-only
// would be a formality.
func TestViewerKeyDoesNotYieldTheInputKey(t *testing.T) {
	root := rootKey(t)
	id := "session-a"
	host := hostSession(t, id, root)

	vk, err := ViewerKey(id, root)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(vk, root) {
		t.Fatal("the viewer key is the root key")
	}

	// Treating the viewer key as a root key derives something else entirely,
	// so input forged that way does not authenticate.
	forger, err := FromRootKey(id, vk)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := forger.SealGuestToHost(0, []byte("injected"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.OpenGuestToHost(0, sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("input forged from a viewer key was accepted: %v", err)
	}
}

// Output must not be replayable as input, or a relay could echo a session's
// own text back at its shell.
func TestDirectionsAreNotInterchangeable(t *testing.T) {
	root := rootKey(t)
	s := hostSession(t, "session-a", root)

	sealed, err := s.SealHostToGuest(0, []byte("whoami"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenGuestToHost(0, sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatal("output was accepted as input")
	}
}

// A frame from a forwarded connection must not be passable as terminal input.
func TestChannelsAreBound(t *testing.T) {
	root := rootKey(t)
	s := hostSession(t, "session-a", root)

	sealed, err := s.SealGuestToHost(protocol.ChannelID(7), []byte("database bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenGuestToHost(protocol.ChannelTerminal, sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatal("a forwarded frame was accepted on the terminal channel")
	}
	if _, err := s.OpenGuestToHost(protocol.ChannelID(7), sealed); err != nil {
		t.Fatalf("the frame did not open on its own channel: %v", err)
	}
}

// Two sessions with the same root key must not share traffic keys.
func TestSessionIDSeparatesKeys(t *testing.T) {
	root := rootKey(t)
	a := hostSession(t, "session-a", root)
	b := hostSession(t, "session-b", root)

	sealed, err := a.SealHostToGuest(0, []byte("for session a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.OpenHostToGuest(0, sealed); !errors.Is(err, ErrDecrypt) {
		t.Fatal("a frame crossed between sessions")
	}
}

// Nonces are random, so the same plaintext must not seal to the same bytes.
func TestSealIsNotDeterministic(t *testing.T) {
	root := rootKey(t)
	s := hostSession(t, "session-a", root)

	seen := make(map[string]struct{}, 500)
	for i := 0; i < 500; i++ {
		sealed, err := s.SealHostToGuest(0, []byte("same input every time"))
		if err != nil {
			t.Fatal(err)
		}
		key := string(sealed)
		if _, dup := seen[key]; dup {
			t.Fatalf("two seals produced identical bytes after %d frames", i)
		}
		seen[key] = struct{}{}
	}
}

// A nil Session is the unencrypted case, and must pass bytes through rather
// than panic — the call sites rely on that to stay free of conditionals.
func TestNilSessionIsPassthrough(t *testing.T) {
	var s *Session
	if s.Enabled() {
		t.Fatal("a nil Session reports that it is enabled")
	}
	if s.CanWrite() {
		t.Fatal("a nil Session reports that it can write")
	}

	payload := []byte("plaintext")
	for _, f := range []func(protocol.ChannelID, []byte) ([]byte, error){
		s.SealHostToGuest, s.OpenHostToGuest, s.SealGuestToHost, s.OpenGuestToHost,
	} {
		got, err := f(0, payload)
		if err != nil {
			t.Fatalf("nil Session returned an error: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("nil Session altered the payload: %v", got)
		}
	}
}

func TestMalformedKeysAreRejected(t *testing.T) {
	for _, n := range []int{0, 1, KeySize - 1, KeySize + 1} {
		if _, err := FromRootKey("s", make([]byte, n)); !errors.Is(err, ErrBadKey) {
			t.Errorf("FromRootKey accepted a %d-byte key", n)
		}
		if _, err := FromViewerKey(make([]byte, n)); !errors.Is(err, ErrBadKey) {
			t.Errorf("FromViewerKey accepted a %d-byte key", n)
		}
		if _, err := ViewerKey("s", make([]byte, n)); !errors.Is(err, ErrBadKey) {
			t.Errorf("ViewerKey accepted a %d-byte key", n)
		}
	}
}

func TestNewRootKeyIsRandom(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		k := rootKey(t)
		if len(k) != KeySize {
			t.Fatalf("key is %d bytes", len(k))
		}
		if bytes.Equal(k, make([]byte, KeySize)) {
			t.Fatal("key is all zeroes")
		}
		if _, dup := seen[string(k)]; dup {
			t.Fatalf("duplicate key after %d draws", i)
		}
		seen[string(k)] = struct{}{}
	}
}

func TestOverheadMatchesReality(t *testing.T) {
	root := rootKey(t)
	s := hostSession(t, "session-a", root)
	pt := []byte("x")
	sealed, err := s.SealHostToGuest(0, pt)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sealed) - len(pt); got != Overhead() {
		t.Fatalf("overhead = %d, Overhead() says %d", got, Overhead())
	}
}

func TestEmptyPayloadRoundTrips(t *testing.T) {
	root := rootKey(t)
	s := hostSession(t, "session-a", root)
	sealed, err := s.SealHostToGuest(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenHostToGuest(0, sealed)
	if err != nil {
		t.Fatalf("an empty payload did not round trip: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bytes back", len(got))
	}
}
