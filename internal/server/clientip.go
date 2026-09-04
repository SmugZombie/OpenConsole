package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Working out who a request came from.
//
// This matters more than it looks. The relay is meant to sit behind a reverse
// proxy, and behind one every request arrives from the proxy's address — so a
// rate limit keyed on the socket address would be one shared bucket for the
// whole internet, which is worse than none at all because it looks like it
// works.
//
// The fix is X-Forwarded-For, but that header is attacker-controlled: anyone
// can claim to be any address, which would let them evade a limit or frame
// someone else for hitting it. So it is honoured only when the connection
// itself came from an address the operator has declared trustworthy.

// TrustedProxies is a set of networks whose forwarding headers are believed.
type TrustedProxies struct {
	nets []netip.Prefix
}

// ParseTrustedProxies reads a comma-separated list of CIDRs or bare addresses.
// An empty string trusts nothing, which is the default.
func ParseTrustedProxies(spec string) (TrustedProxies, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return TrustedProxies{}, nil
	}

	var tp TrustedProxies
	for _, raw := range strings.Split(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if p, err := netip.ParsePrefix(entry); err == nil {
			tp.nets = append(tp.nets, p)
			continue
		}
		// A bare address is the same as a single-host prefix.
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return TrustedProxies{}, fmt.Errorf(
				"invalid trusted proxy %q: expected an address or CIDR", entry)
		}
		tp.nets = append(tp.nets, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return tp, nil
}

// Empty reports whether any proxy is trusted.
func (t TrustedProxies) Empty() bool { return len(t.nets) == 0 }

// trusts reports whether addr is a proxy whose headers should be believed.
func (t TrustedProxies) trusts(addr netip.Addr) bool {
	for _, n := range t.nets {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// ClientIP returns the address a request should be attributed to.
//
// It walks X-Forwarded-For from the right, skipping entries contributed by
// trusted proxies, and returns the first address that is not one of them: the
// closest hop the operator has not vouched for. Taking the leftmost entry
// instead — the common mistake — takes whatever the client wrote down.
func (t TrustedProxies) ClientIP(r *http.Request) string {
	peer, err := netip.ParseAddr(hostOnly(r.RemoteAddr))
	if err != nil {
		return hostOnly(r.RemoteAddr)
	}
	if t.Empty() || !t.trusts(peer) {
		// Either nothing is trusted, or this connection did not come from a
		// proxy we believe. Its own address is the only fact available.
		return peer.String()
	}

	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return peer.String()
	}

	// Flatten, because a header may appear more than once and each may hold a
	// comma-separated list.
	var hops []string
	for _, h := range forwarded {
		for _, part := range strings.Split(h, ",") {
			if v := strings.TrimSpace(part); v != "" {
				hops = append(hops, v)
			}
		}
	}

	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hostOnly(hops[i]))
		if err != nil {
			// A malformed entry ends the chain: everything to its left was
			// written by something we cannot reason about.
			break
		}
		if !t.trusts(addr) {
			return addr.String()
		}
	}
	return peer.String()
}

// hostOnly strips a port and any brackets from an address.
func hostOnly(s string) string {
	s = strings.TrimSpace(s)
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return strings.Trim(s, "[]")
}
