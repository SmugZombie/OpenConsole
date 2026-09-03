package session

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestRingBuffer(t *testing.T) {
	tests := []struct {
		name   string
		size   int
		writes []string
		want   string
	}{
		{"empty", 8, nil, ""},
		{"under capacity", 8, []string{"abc"}, "abc"},
		{"exactly capacity", 4, []string{"abcd"}, "abcd"},
		{"several writes under capacity", 8, []string{"ab", "cd"}, "abcd"},
		{"overflow by one", 4, []string{"abcde"}, "bcde"},
		{"write larger than buffer", 3, []string{"abcdefg"}, "efg"},
		{"wrap across writes", 4, []string{"abc", "de"}, "bcde"},
		{"many small writes", 4, []string{"a", "b", "c", "d", "e", "f"}, "cdef"},
		{"repeated overflow", 3, []string{"abcd", "efgh"}, "fgh"},
		{"zero size", 0, []string{"abc"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRingBuffer(tc.size)
			for _, w := range tc.writes {
				r.Write([]byte(w))
			}
			if got := string(r.Bytes()); got != tc.want {
				t.Fatalf("Bytes() = %q, want %q", got, tc.want)
			}
			if r.Len() != len(tc.want) {
				t.Fatalf("Len() = %d, want %d", r.Len(), len(tc.want))
			}
		})
	}
}

// TestRingBufferMatchesTail cross-checks the ring against the obvious slow
// implementation over random writes.
func TestRingBufferMatchesTail(t *testing.T) {
	const size = 64
	rng := rand.New(rand.NewSource(1))
	r := newRingBuffer(size)
	var want []byte

	for i := 0; i < 500; i++ {
		p := make([]byte, rng.Intn(100))
		for j := range p {
			p[j] = byte('a' + rng.Intn(26))
		}
		r.Write(p)

		want = append(want, p...)
		if len(want) > size {
			want = want[len(want)-size:]
		}
		if got := r.Bytes(); !bytes.Equal(got, want) {
			t.Fatalf("iteration %d: got %q, want %q", i, got, want)
		}
	}
}
