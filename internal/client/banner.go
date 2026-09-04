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
func printBanner(w io.Writer, cfg Config, sess *Session, joinURL, viewURL, sshCmd string) {
	ticket := Ticket(sess.SessionID, sess.GuestToken)
	viewTicket := Ticket(sess.SessionID, sess.ViewerToken)

	fmt.Fprintf(w, "\nopenconsole: sharing this terminal\n")
	fmt.Fprintf(w, "  relay:    %s\n", cfg.Server)
	fmt.Fprintf(w, "  session:  %s\n", sess.SessionID)
	fmt.Fprintf(w, "  expires:  %s\n", humanDuration(time.Until(sess.ExpiresAt)))
	fmt.Fprintf(w, "\n  in a browser:\n    %s\n", joinURL)
	fmt.Fprintf(w, "\n  in a terminal:\n    openconsole join %s\n", ticket)
	if cfg.Server != DefaultServer {
		fmt.Fprintf(w, "      (with -server %s, or OPENCONSOLE_SERVER)\n", cfg.Server)
	}
	if sshCmd != "" {
		// The token is not on this command line on purpose: ssh prompts for
		// it, so it stays out of the guest's shell history and out of `ps`.
		fmt.Fprintf(w, "\n  with any ssh client:\n    %s\n", sshCmd)
		fmt.Fprintf(w, "      (paste the part after the dot as the token)\n")
	}
	fmt.Fprintf(w, "\n  watch only (cannot type):\n    %s\n", viewURL)
	fmt.Fprintf(w, "    openconsole join %s\n", viewTicket)

	fmt.Fprintf(w, "\n  The first ticket grants full control of this terminal; the second lets\n")
	fmt.Fprintf(w, "  someone watch only. Send either one privately, and type 'exit' here to\n")
	fmt.Fprintf(w, "  end the session.\n\n")
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
