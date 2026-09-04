package e2e

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/SmugZombie/OpenConsole/internal/protocol"
)

// The Go and browser implementations are two halves of one wire format, and a
// disagreement about any of it — the HKDF info strings, the salt, the nonce
// layout, the additional data — would show up as a browser that silently
// cannot read the terminal.
//
// testdata/interop.json holds frames sealed by each side. This test opens the
// ones WebCrypto produced; web/src/crypto.test.ts opens the ones produced
// here. Neither can drift without the other noticing.
type interopFixture struct {
	SessionID    string `json:"sessionId"`
	RootKeyHex   string `json:"rootKeyHex"`
	ViewerKeyHex string `json:"viewerKeyHex"`
	Cases        []struct {
		Name          string `json:"name"`
		Channel       uint32 `json:"channel"`
		Plaintext     string `json:"plaintext"`
		GoHostSealed  string `json:"goHostSealed"`
		TSGuestSealed string `json:"tsGuestSealed"`
		TSHostSealed  string `json:"tsHostSealed"`
	} `json:"cases"`
}

func loadFixture(t *testing.T) interopFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/interop.json")
	if err != nil {
		t.Fatalf("reading the interop fixture: %v", err)
	}
	var fx interopFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parsing the interop fixture: %v", err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("the interop fixture has no cases")
	}
	return fx
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex in the fixture: %v", err)
	}
	return b
}

// A browser guest's keystrokes have to reach the host's shell.
func TestOpensFramesSealedByTheBrowser(t *testing.T) {
	fx := loadFixture(t)
	s, err := FromRootKey(fx.SessionID, mustHex(t, fx.RootKeyHex))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range fx.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if tc.TSGuestSealed == "" {
				t.Skip("no browser-sealed frame for this case")
			}
			want := mustHex(t, tc.Plaintext)

			got, err := s.OpenGuestToHost(protocol.ChannelID(tc.Channel), mustHex(t, tc.TSGuestSealed))
			if err != nil {
				t.Fatalf("could not open a frame WebCrypto sealed: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("opened %x, want %x", got, want)
			}

			// And the other direction's layout, so a mismatch cannot hide in
			// the half this test does not normally exercise.
			got, err = s.OpenHostToGuest(protocol.ChannelID(tc.Channel), mustHex(t, tc.TSHostSealed))
			if err != nil {
				t.Fatalf("could not open a host-direction frame from WebCrypto: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("opened %x, want %x", got, want)
			}
		})
	}
}

// The viewer key each side derives has to be the same one, or a watch-only
// link opened in a browser shows noise.
func TestViewerKeyMatchesTheBrowser(t *testing.T) {
	fx := loadFixture(t)
	got, err := ViewerKey(fx.SessionID, mustHex(t, fx.RootKeyHex))
	if err != nil {
		t.Fatal(err)
	}
	if want := mustHex(t, fx.ViewerKeyHex); !bytes.Equal(got, want) {
		t.Fatalf("viewer key = %x, fixture says %x", got, want)
	}
}

// The fixture is only meaningful if the frames in it are the ones this code
// would produce, so check the Go half opens with the current implementation.
func TestFixtureMatchesCurrentImplementation(t *testing.T) {
	fx := loadFixture(t)
	s, err := FromRootKey(fx.SessionID, mustHex(t, fx.RootKeyHex))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range fx.Cases {
		got, err := s.OpenHostToGuest(protocol.ChannelID(tc.Channel), mustHex(t, tc.GoHostSealed))
		if err != nil {
			t.Fatalf("%s: the committed Go frame no longer opens: %v", tc.Name, err)
		}
		if want := mustHex(t, tc.Plaintext); !bytes.Equal(got, want) {
			t.Fatalf("%s: opened %x, want %x", tc.Name, got, want)
		}
	}
}
