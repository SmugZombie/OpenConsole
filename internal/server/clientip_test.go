package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func request(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// With nothing trusted, the socket address is the only fact available and a
// forwarding header must not be able to override it.
func TestClientIPIgnoresHeadersFromUntrustedPeers(t *testing.T) {
	var none TrustedProxies
	if !none.Empty() {
		t.Fatal("the zero value should trust nothing")
	}

	got := none.ClientIP(request("203.0.113.9:1234", "1.1.1.1"))
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want the peer address", got)
	}
}

func TestClientIPHonoursTrustedProxy(t *testing.T) {
	tp, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}

	// The proxy is trusted, so its record of the real client is believed.
	got := tp.ClientIP(request("10.0.0.5:443", "198.51.100.7"))
	if got != "198.51.100.7" {
		t.Fatalf("ClientIP = %q, want the forwarded client", got)
	}

	// The same header from an untrusted peer is ignored.
	got = tp.ClientIP(request("203.0.113.9:443", "198.51.100.7"))
	if got != "203.0.113.9" {
		t.Fatalf("ClientIP = %q, want the peer address", got)
	}
}

// The header is attacker-controlled. Taking the leftmost entry — the common
// mistake — lets anyone claim any address and evade a per-source limit.
func TestClientIPDoesNotTrustSpoofedPrefix(t *testing.T) {
	tp, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}

	// A client wrote "1.1.1.1"; the proxy appended the address it really saw.
	got := tp.ClientIP(request("10.0.0.5:443", "1.1.1.1, 198.51.100.7"))
	if got != "198.51.100.7" {
		t.Fatalf("ClientIP = %q, want the address the proxy observed, not the claim", got)
	}
	if got == "1.1.1.1" {
		t.Fatal("a spoofed leftmost entry was believed")
	}
}

func TestClientIPWalksAChainOfTrustedProxies(t *testing.T) {
	tp, err := ParseTrustedProxies("10.0.0.0/8,192.168.0.0/16")
	if err != nil {
		t.Fatal(err)
	}

	// Two hops we vouch for; the first address outside them is the client.
	got := tp.ClientIP(request("10.0.0.5:443", "198.51.100.7, 192.168.1.1"))
	if got != "198.51.100.7" {
		t.Fatalf("ClientIP = %q", got)
	}
}

func TestClientIPHandlesRepeatedHeaders(t *testing.T) {
	tp, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	got := tp.ClientIP(request("10.0.0.5:443", "198.51.100.7", "10.0.0.9"))
	if got != "198.51.100.7" {
		t.Fatalf("ClientIP = %q", got)
	}
}

func TestClientIPHandlesMalformedHeader(t *testing.T) {
	tp, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	// Garbage ends the chain rather than being attributed to anyone.
	got := tp.ClientIP(request("10.0.0.5:443", "not-an-address"))
	if got != "10.0.0.5" {
		t.Fatalf("ClientIP = %q, want the peer", got)
	}
}

func TestClientIPIPv6(t *testing.T) {
	tp, err := ParseTrustedProxies("::1/128")
	if err != nil {
		t.Fatal(err)
	}
	got := tp.ClientIP(request("[::1]:443", "2001:db8::1"))
	if got != "2001:db8::1" {
		t.Fatalf("ClientIP = %q", got)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	tp, err := ParseTrustedProxies("10.0.0.1, 172.16.0.0/12, ::1")
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if tp.Empty() {
		t.Fatal("nothing was parsed")
	}

	if _, err := ParseTrustedProxies(""); err != nil {
		t.Fatalf("an empty spec should be fine: %v", err)
	}
	for _, bad := range []string{"nonsense", "10.0.0.0/99", "300.1.1.1"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Errorf("ParseTrustedProxies(%q) should have failed", bad)
		}
	}
}
