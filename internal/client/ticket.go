package client

import (
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/SmugZombie/OpenConsole/internal/e2e"
)

// A ticket is what one person sends another. It carries everything needed to
// join and nothing the relay is entitled to:
//
//	<session-id>.<token>.k<root-key>      full access
//	<session-id>.<token>.v<viewer-key>    watch only
//	<session-id>.<token>                  no encryption
//
// The relay is told the first two fields, because it uses them to authenticate
// the connection. It is never told the third. That is the whole basis of the
// encryption: the key exists only in this string, which travels by whatever
// private channel the two people already use, and in a URL fragment, which
// browsers do not transmit.
//
// The separator is "." because every field is lowercase base32, which never
// contains one. A ticket grants what its letter says and should be sent
// privately either way.

const ticketSep = "."

// KeyKind says what a ticket's key is for. The letter is on the wire so a
// client knows what it holds without asking the relay — which must not be able
// to talk a guest into misusing its own key.
type KeyKind byte

const (
	// KeyNone means the session is not encrypted.
	KeyNone KeyKind = 0
	// KeyRoot derives both directions: read the terminal and type into it.
	KeyRoot KeyKind = 'k'
	// KeyViewer derives only the direction that reads.
	KeyViewer KeyKind = 'v'
)

// keyEncoding matches the one used for session IDs and tokens: unpadded
// lowercase base32, so a whole ticket can be read aloud or typed.
var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Ticket is a parsed join credential.
type Ticket struct {
	SessionID string
	Token     string
	// Key is nil when the session is not encrypted.
	Key     []byte
	KeyKind KeyKind
}

// Encrypted reports whether the ticket carries a key.
func (t Ticket) Encrypted() bool { return len(t.Key) > 0 && t.KeyKind != KeyNone }

// String renders the ticket for sharing.
func (t Ticket) String() string {
	s := t.SessionID + ticketSep + t.Token
	if !t.Encrypted() {
		return s
	}
	return s + ticketSep + string(t.KeyKind) + strings.ToLower(keyEncoding.EncodeToString(t.Key))
}

// Fragment renders the part of a join URL that a browser never transmits.
func (t Ticket) Fragment() string {
	if !t.Encrypted() {
		return t.Token
	}
	return t.Token + ticketSep + string(t.KeyKind) +
		strings.ToLower(keyEncoding.EncodeToString(t.Key))
}

// E2E builds the encryption state this ticket allows, or nil when the session
// is not encrypted.
func (t Ticket) E2E() (*e2e.Session, error) {
	switch t.KeyKind {
	case KeyNone:
		return nil, nil
	case KeyRoot:
		return e2e.FromRootKey(t.SessionID, t.Key)
	case KeyViewer:
		return e2e.FromViewerKey(t.Key)
	default:
		return nil, fmt.Errorf("invalid ticket: unknown key type %q", string(t.KeyKind))
	}
}

// ParseTicket reads a ticket.
func ParseTicket(s string) (Ticket, error) {
	fields := strings.Split(strings.TrimSpace(s), ticketSep)
	if len(fields) < 2 || len(fields) > 3 {
		return Ticket{}, fmt.Errorf(
			"invalid ticket: expected <session>%s<token> with an optional key", ticketSep)
	}

	t := Ticket{SessionID: fields[0], Token: fields[1]}
	if t.SessionID == "" || t.Token == "" {
		return Ticket{}, fmt.Errorf("invalid ticket: it is missing a session or a token")
	}
	if len(fields) == 2 {
		return t, nil
	}

	key, kind, err := parseKeyField(fields[2])
	if err != nil {
		return Ticket{}, err
	}
	t.Key, t.KeyKind = key, kind
	return t, nil
}

// ParseFragment reads the part of a join URL after the '#'.
func ParseFragment(sessionID, fragment string) (Ticket, error) {
	fields := strings.Split(strings.TrimSpace(fragment), ticketSep)
	if len(fields) < 1 || len(fields) > 2 || fields[0] == "" {
		return Ticket{}, fmt.Errorf("invalid link: the part after the # is malformed")
	}

	t := Ticket{SessionID: sessionID, Token: fields[0]}
	if len(fields) == 1 {
		return t, nil
	}
	key, kind, err := parseKeyField(fields[1])
	if err != nil {
		return Ticket{}, err
	}
	t.Key, t.KeyKind = key, kind
	return t, nil
}

// parseKeyField reads the letter and the encoded key.
func parseKeyField(field string) ([]byte, KeyKind, error) {
	if field == "" {
		return nil, KeyNone, fmt.Errorf("invalid ticket: the key is empty")
	}
	kind := KeyKind(field[0])
	if kind != KeyRoot && kind != KeyViewer {
		return nil, KeyNone, fmt.Errorf("invalid ticket: unknown key type %q", string(field[0]))
	}

	raw, err := keyEncoding.DecodeString(strings.ToUpper(field[1:]))
	if err != nil {
		return nil, KeyNone, fmt.Errorf("invalid ticket: the key is not readable")
	}
	if len(raw) != e2e.KeySize {
		return nil, KeyNone, fmt.Errorf(
			"invalid ticket: the key is %d bytes, expected %d", len(raw), e2e.KeySize)
	}
	return raw, kind, nil
}
