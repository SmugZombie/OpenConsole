package client

import "testing"

func TestAllowlistDeniesByDefault(t *testing.T) {
	// Forwarding reaches whatever the host can reach, so silence must mean no.
	var zero Allowlist
	if zero.Enabled() {
		t.Fatal("the zero Allowlist permits forwarding")
	}
	if zero.Allows("localhost", 5432) {
		t.Fatal("the zero Allowlist allowed a target")
	}

	empty, err := ParseAllowlist("")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	if empty.Enabled() || empty.Allows("localhost", 5432) {
		t.Fatal("an empty spec permitted forwarding")
	}
	if empty.String() != "none" {
		t.Fatalf("String() = %q", empty.String())
	}
}

func TestAllowlistExactTargets(t *testing.T) {
	list, err := ParseAllowlist("localhost:5432, 127.0.0.1:8080")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	if !list.Enabled() || list.AllowsAny() {
		t.Fatal("expected a non-wildcard, enabled list")
	}

	allowed := []struct {
		host string
		port uint16
	}{
		{"localhost", 5432},
		{"LocalHost", 5432}, // case is not significant in a host name
		{"127.0.0.1", 8080},
	}
	for _, tc := range allowed {
		if !list.Allows(tc.host, tc.port) {
			t.Errorf("Allows(%s, %d) = false, want true", tc.host, tc.port)
		}
	}

	denied := []struct {
		host string
		port uint16
	}{
		{"localhost", 5433},      // a neighbouring port is a different service
		{"localhost", 8080},      // host and port are matched as a pair
		{"127.0.0.1", 5432},      // and an address is not its name
		{"evil.example.com", 80}, // anything unlisted
		{"169.254.169.254", 80},  // cloud metadata, the classic target
	}
	for _, tc := range denied {
		if list.Allows(tc.host, tc.port) {
			t.Errorf("Allows(%s, %d) = true, want false", tc.host, tc.port)
		}
	}
}

func TestAllowlistWildcard(t *testing.T) {
	list, err := ParseAllowlist("any")
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	if !list.Enabled() || !list.AllowsAny() {
		t.Fatal("the wildcard did not take effect")
	}
	if !list.Allows("anything.example.com", 65535) {
		t.Fatal("the wildcard denied a target")
	}
	if list.String() != AllowAny {
		t.Fatalf("String() = %q", list.String())
	}
}

func TestAllowlistRejectsMalformed(t *testing.T) {
	for _, spec := range []string{
		"localhost",        // no port
		"localhost:",       // empty port
		":5432",            // no host
		"localhost:0",      // port 0 is not dialable
		"localhost:70000",  // out of range
		"localhost:notnum", // not a number
		"host:1:2",         // not a host:port at all
	} {
		if _, err := ParseAllowlist(spec); err == nil {
			t.Errorf("ParseAllowlist(%q) should have failed", spec)
		}
	}
}

func TestAllowlistStringIsStable(t *testing.T) {
	// The banner should read the same on every run.
	a, err := ParseAllowlist("z.example:2,a.example:1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := a.String(), "a.example:1, z.example:2"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
