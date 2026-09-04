package client

import (
	"bytes"
	"testing"

	"github.com/SmugZombie/OpenConsole/internal/e2e"
)

func TestTicketRoundTripWithoutAKey(t *testing.T) {
	want := Ticket{SessionID: "x5s5gzxptgfksy3hu75jmcoltm", Token: "3u2avt7nibb2oxbztfxkcbj7yy"}

	got, err := ParseTicket(want.String())
	if err != nil {
		t.Fatalf("ParseTicket(%q): %v", want.String(), err)
	}
	if got.SessionID != want.SessionID || got.Token != want.Token {
		t.Fatalf("round trip gave %+v", got)
	}
	if got.Encrypted() {
		t.Fatal("a two-field ticket reports that it is encrypted")
	}
}

func TestTicketRoundTripWithAKey(t *testing.T) {
	root, err := e2e.NewRootKey()
	if err != nil {
		t.Fatal(err)
	}
	want := Ticket{
		SessionID: "x5s5gzxptgfksy3hu75jmcoltm",
		Token:     "3u2avt7nibb2oxbztfxkcbj7yy",
		Key:       root,
		KeyKind:   KeyRoot,
	}

	got, err := ParseTicket(want.String())
	if err != nil {
		t.Fatalf("ParseTicket: %v", err)
	}
	if got.SessionID != want.SessionID || got.Token != want.Token {
		t.Fatalf("round trip gave %+v", got)
	}
	if !bytes.Equal(got.Key, root) {
		t.Fatal("the key did not survive the round trip")
	}
	if got.KeyKind != KeyRoot || !got.Encrypted() {
		t.Fatalf("kind = %q", string(got.KeyKind))
	}
}

// The letter is in the ticket so a client knows what it holds without asking
// the relay, which must not be able to talk a guest into misusing its key.
func TestTicketCarriesItsKeyKind(t *testing.T) {
	root, err := e2e.NewRootKey()
	if err != nil {
		t.Fatal(err)
	}
	full := Ticket{SessionID: "s", Token: "t", Key: root, KeyKind: KeyRoot}
	view := Ticket{SessionID: "s", Token: "t", Key: root, KeyKind: KeyViewer}

	if full.String() == view.String() {
		t.Fatal("a full and a watch-only ticket render identically")
	}

	parsedFull, err := ParseTicket(full.String())
	if err != nil {
		t.Fatal(err)
	}
	parsedView, err := ParseTicket(view.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsedFull.KeyKind != KeyRoot || parsedView.KeyKind != KeyViewer {
		t.Fatalf("kinds came back as %q and %q",
			string(parsedFull.KeyKind), string(parsedView.KeyKind))
	}

	// And they build different capabilities.
	fullSess, err := parsedFull.E2E()
	if err != nil {
		t.Fatal(err)
	}
	viewSess, err := parsedView.E2E()
	if err != nil {
		t.Fatal(err)
	}
	if !fullSess.CanWrite() {
		t.Fatal("a full ticket cannot write")
	}
	if viewSess.CanWrite() {
		t.Fatal("a watch-only ticket can write")
	}
}

func TestFragmentRoundTrip(t *testing.T) {
	root, err := e2e.NewRootKey()
	if err != nil {
		t.Fatal(err)
	}
	want := Ticket{SessionID: "abc123", Token: "tok", Key: root, KeyKind: KeyRoot}

	got, err := ParseFragment("abc123", want.Fragment())
	if err != nil {
		t.Fatalf("ParseFragment(%q): %v", want.Fragment(), err)
	}
	if got.Token != want.Token || !bytes.Equal(got.Key, root) || got.KeyKind != KeyRoot {
		t.Fatalf("round trip gave %+v", got)
	}

	// A link from an unencrypted session is just the token.
	plain := Ticket{SessionID: "abc123", Token: "tok"}
	got, err = ParseFragment("abc123", plain.Fragment())
	if err != nil {
		t.Fatal(err)
	}
	if got.Encrypted() {
		t.Fatal("a bare fragment produced a key")
	}
}

func TestParseTicketTrimsWhitespace(t *testing.T) {
	// Tickets get copied out of chat clients, which love adding whitespace.
	got, err := ParseTicket("  abc.def\n")
	if err != nil {
		t.Fatalf("ParseTicket: %v", err)
	}
	if got.SessionID != "abc" || got.Token != "def" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseTicketRejectsMalformed(t *testing.T) {
	bad := []string{
		"", "   ", "no-separator", ".", "abc.", ".def",
		"a.b.c.d",        // too many fields
		"a.b.",           // empty key field
		"a.b.x" + "aaaa", // unknown key letter
		"a.b.k",          // key letter with nothing after it
		"a.b.k!!!!",      // not base32
		"a.b.kaaaa",      // right shape, wrong length
	}
	for _, in := range bad {
		if _, err := ParseTicket(in); err == nil {
			t.Errorf("ParseTicket(%q) should have failed", in)
		}
	}
}

// A key of the wrong length must not reach the cipher.
func TestParseTicketRejectsShortKey(t *testing.T) {
	short := Ticket{SessionID: "s", Token: "t", Key: make([]byte, e2e.KeySize-1), KeyKind: KeyRoot}
	if _, err := ParseTicket(short.String()); err == nil {
		t.Fatal("a short key was accepted")
	}
}
