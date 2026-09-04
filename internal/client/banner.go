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
func printBanner(w io.Writer, cfg Config, sess *Session, ticket Ticket, api *Client) {
	viewTicket, err := viewerTicket(ticket, sess)
	if err != nil {
		// A viewer credential that cannot be built is not fatal to the
		// session, so say so and carry on with the full one.
		fmt.Fprintf(w, "openconsole: could not build a watch-only ticket: %v\n", err)
	}

	fmt.Fprintf(w, "\nopenconsole: sharing this terminal\n")
	fmt.Fprintf(w, "  relay:    %s\n", cfg.Server)
	fmt.Fprintf(w, "  session:  %s\n", sess.SessionID)
	fmt.Fprintf(w, "  expires:  %s\n", humanDuration(time.Until(sess.ExpiresAt)))
	if ticket.Encrypted() {
		fmt.Fprintf(w, "  privacy:  end-to-end encrypted; the relay cannot read this terminal\n")
	} else {
		fmt.Fprintf(w, "  privacy:  NOT encrypted; whoever runs the relay can read and type here\n")
	}

	fmt.Fprintf(w, "\n  in a browser:\n    %s\n", api.JoinURL(ticket))
	fmt.Fprintf(w, "\n  in a terminal:\n    openconsole join %s\n", ticket)
	if cfg.Server != DefaultServer {
		fmt.Fprintf(w, "      (with -server %s, or OPENCONSOLE_SERVER)\n", cfg.Server)
	}

	if sshCmd := api.SSHCommand(sess.SessionID, sess); sshCmd != "" {
		if ticket.Encrypted() {
			// Say why rather than leaving someone to discover that ssh
			// produces a screen of noise.
			fmt.Fprintf(w, "\n  ssh joins are not available: an ssh client cannot decrypt.\n")
			fmt.Fprintf(w, "    Share with -no-encryption to allow them.\n")
		} else {
			// The token is not on this command line on purpose: ssh prompts
			// for it, so it stays out of the guest's shell history and out
			// of `ps`.
			fmt.Fprintf(w, "\n  with any ssh client:\n    %s\n", sshCmd)
			fmt.Fprintf(w, "      (paste the part after the dot as the token)\n")
		}
	}

	if err == nil {
		fmt.Fprintf(w, "\n  watch only (cannot type):\n    %s\n", api.JoinURL(viewTicket))
		fmt.Fprintf(w, "    openconsole join %s\n", viewTicket)
	}

	if cfg.AllowForward.Enabled() {
		fmt.Fprintf(w, "\n  port forwarding: guests may reach %s\n", cfg.AllowForward.String())
		if cfg.AllowForward.AllowsAny() {
			// Worth saying plainly: this is every address this machine can
			// reach, not just the obvious ones.
			fmt.Fprintf(w, "    WARNING: that is anything this machine can reach, including\n")
			fmt.Fprintf(w, "    private networks and cloud metadata endpoints.\n")
		}
	}

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
