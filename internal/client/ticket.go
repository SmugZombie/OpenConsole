package client

import (
	"fmt"
	"strings"
)

// A ticket bundles the two things a guest needs — the public session ID and the
// guest token — into one copy-pasteable string.
//
// The separator is "." because both halves are lowercase base32, which never
// contains a dot. The token half is a credential: a ticket grants full
// interactive access to the shared terminal and should be sent over a private
// channel, not posted somewhere public.

const ticketSep = "."

// Ticket formats a session ID and guest token for sharing.
func Ticket(sessionID, guestToken string) string {
	return sessionID + ticketSep + guestToken
}

// ParseTicket splits a ticket back into its parts.
func ParseTicket(s string) (sessionID, guestToken string, err error) {
	s = strings.TrimSpace(s)
	id, token, ok := strings.Cut(s, ticketSep)
	if !ok || id == "" || token == "" {
		return "", "", fmt.Errorf("invalid ticket: expected <session>%s<token>", ticketSep)
	}
	if strings.Contains(token, ticketSep) {
		return "", "", fmt.Errorf("invalid ticket: too many %q separators", ticketSep)
	}
	return id, token, nil
}
