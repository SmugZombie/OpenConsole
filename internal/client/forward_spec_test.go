package client

import "testing"

func TestParseForwardSpec(t *testing.T) {
	tests := []struct {
		in   string
		want ForwardSpec
	}{
		// The common form: a local port, and where the host should connect.
		{"8080:localhost:80", ForwardSpec{"127.0.0.1:8080", "localhost", 80}},
		// Spelling out the bind address changes nothing.
		{"127.0.0.1:8080:localhost:80", ForwardSpec{"127.0.0.1:8080", "localhost", 80}},
		// Binding wider has to be deliberate.
		{"0.0.0.0:8080:db.internal:5432", ForwardSpec{"0.0.0.0:8080", "db.internal", 5432}},
		{" 5432:127.0.0.1:5432 ", ForwardSpec{"127.0.0.1:5432", "127.0.0.1", 5432}},
	}
	for _, tc := range tests {
		got, err := ParseForwardSpec(tc.in)
		if err != nil {
			t.Errorf("ParseForwardSpec(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseForwardSpec(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// A two-part spec must bind loopback: a forward is a hole into someone else's
// network, and exposing it to the local network by default would be a surprise.
func TestParseForwardSpecDefaultsToLoopback(t *testing.T) {
	got, err := ParseForwardSpec("9000:localhost:9000")
	if err != nil {
		t.Fatal(err)
	}
	if got.ListenAddr != "127.0.0.1:9000" {
		t.Fatalf("ListenAddr = %q, want loopback", got.ListenAddr)
	}
}

func TestParseForwardSpecRejectsMalformed(t *testing.T) {
	for _, spec := range []string{
		"",
		"8080",
		"8080:localhost",
		"8080:localhost:80:extra:more",
		"0:localhost:80",
		"8080:localhost:0",
		"8080:localhost:70000",
		"notanumber:localhost:80",
		"8080::80",
	} {
		if _, err := ParseForwardSpec(spec); err == nil {
			t.Errorf("ParseForwardSpec(%q) should have failed", spec)
		}
	}
}

func TestForwardListCollectsFlags(t *testing.T) {
	var list forwardList
	if err := list.Set("8080:localhost:80"); err != nil {
		t.Fatal(err)
	}
	if err := list.Set("5432:db:5432"); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("collected %d forwards, want 2", len(list))
	}
	if err := list.Set("nonsense"); err == nil {
		t.Fatal("a malformed -L was accepted")
	}
}
