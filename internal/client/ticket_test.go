package client

import "testing"

func TestTicketRoundTrip(t *testing.T) {
	id, token := "x5s5gzxptgfksy3hu75jmcoltm", "3u2avt7nibb2oxbztfxkcbj7yy"
	got := Ticket(id, token)

	gotID, gotToken, err := ParseTicket(got)
	if err != nil {
		t.Fatalf("ParseTicket(%q): %v", got, err)
	}
	if gotID != id || gotToken != token {
		t.Fatalf("round trip gave (%q, %q), want (%q, %q)", gotID, gotToken, id, token)
	}
}

func TestParseTicketTrimsWhitespace(t *testing.T) {
	// Tickets get copied out of chat clients, which love adding whitespace.
	id, token, err := ParseTicket("  abc.def\n")
	if err != nil {
		t.Fatalf("ParseTicket: %v", err)
	}
	if id != "abc" || token != "def" {
		t.Fatalf("got (%q, %q)", id, token)
	}
}

func TestParseTicketRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "   ", "no-separator", ".", "abc.", ".def", "a.b.c"} {
		if _, _, err := ParseTicket(in); err == nil {
			t.Fatalf("ParseTicket(%q) should have failed", in)
		}
	}
}
