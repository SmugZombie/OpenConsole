package client

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// printBanner tells the host what to pass on to whoever is joining.
//
// It goes to stderr so that redirecting the shared shell's output does not
// capture, and thereby log, the ticket.
func printBanner(w io.Writer, cfg Config, sess *Session, joinURL string) {
	ticket := Ticket(sess.SessionID, sess.GuestToken)

	fmt.Fprintf(w, "\nopenconsole: sharing this terminal\n")
	fmt.Fprintf(w, "  relay:    %s\n", cfg.Server)
	fmt.Fprintf(w, "  session:  %s\n", sess.SessionID)
	fmt.Fprintf(w, "  expires:  %s\n", humanDuration(time.Until(sess.ExpiresAt)))
	fmt.Fprintf(w, "\n  in a browser:\n    %s\n", joinURL)
	fmt.Fprintf(w, "\n  in a terminal:\n    openconsole join %s\n", ticket)
	if cfg.Server != DefaultServer {
		fmt.Fprintf(w, "      (with -server %s, or OPENCONSOLE_SERVER)\n", cfg.Server)
	}
	fmt.Fprintf(w, "\n  The ticket grants full control of this terminal. Send it privately,\n")
	fmt.Fprintf(w, "  and type 'exit' here to end the session.\n\n")
}

// humanDuration renders a duration the way a person would say it.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60

	var parts []string
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 || h == 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	return "in " + strings.Join(parts, " ")
}
