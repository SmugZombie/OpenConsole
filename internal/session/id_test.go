package session

import (
	"strings"
	"testing"
)

func TestNewIDShapeAndUniqueness(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if len(id) != IDLength {
			t.Fatalf("NewID length = %d, want %d (%q)", len(id), IDLength, id)
		}
		if !ValidID(id) {
			t.Fatalf("NewID produced an id ValidID rejects: %q", id)
		}
		if id != strings.ToLower(id) {
			t.Fatalf("NewID returned mixed case: %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID returned a duplicate after %d draws: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewTokenShapeAndUniqueness(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if a == b {
		t.Fatal("NewToken returned the same value twice")
	}
	// 32 bytes of base32 without padding.
	if want := 52; len(a) != want {
		t.Fatalf("token length = %d, want %d", len(a), want)
	}
}

func TestValidID(t *testing.T) {
	good, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}

	tests := []struct {
		name string
		id   string
		want bool
	}{
		{"generated", good, true},
		{"empty", "", false},
		{"too short", good[:len(good)-1], false},
		{"too long", good + "a", false},
		{"uppercase", strings.ToUpper(good), false},
		{"path traversal", strings.Repeat(".", IDLength), false},
		{"slash", strings.Repeat("/", IDLength), false},
		{"base32 excluded digits", strings.Repeat("1", IDLength), false},
		{"null byte", strings.Repeat("\x00", IDLength), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidID(tc.id); got != tc.want {
				t.Fatalf("ValidID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
