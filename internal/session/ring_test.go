package session

import (
	"bytes"
	"math/rand"
	"testing"
)

// join concatenates the retained frames, for the cases where only the byte
// content matters.
func join(r *frameRing) string {
	var out []byte
	for _, f := range r.Frames() {
		out = append(out, f...)
	}
	return string(out)
}

func TestFrameRing(t *testing.T) {
	tests := []struct {
		name       string
		maxBytes   int
		writes     []string
		want       string
		wantFrames int
	}{
		{"empty", 8, nil, "", 0},
		{"under capacity", 8, []string{"abc"}, "abc", 1},
		{"exactly capacity", 4, []string{"abcd"}, "abcd", 1},
		{"several writes under capacity", 8, []string{"ab", "cd"}, "abcd", 2},
		// Eviction is by whole frames, because half a sealed frame is useless.
		{"evicts the oldest frame", 4, []string{"abc", "de"}, "de", 1},
		{"frame larger than the buffer replaces it", 3, []string{"ab", "cdefgh"}, "cdefgh", 1},
		{"many small writes", 4, []string{"a", "b", "c", "d", "e", "f"}, "cdef", 4},
		{"zero capacity", 0, []string{"abc"}, "", 0},
		{"empty write is ignored", 8, []string{"ab", "", "cd"}, "abcd", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newFrameRing(tc.maxBytes)
			for _, w := range tc.writes {
				r.Add([]byte(w))
			}
			if got := join(r); got != tc.want {
				t.Fatalf("contents = %q, want %q", got, tc.want)
			}
			if got := r.Count(); got != tc.wantFrames {
				t.Fatalf("frames = %d, want %d", got, tc.wantFrames)
			}
			if got := r.Len(); got != len(tc.want) {
				t.Fatalf("Len = %d, want %d", got, len(tc.want))
			}
		})
	}
}

// The reason this stores frames rather than bytes: each one is independently
// sealed, and two joined together will never decrypt.
func TestFrameRingPreservesFrameBoundaries(t *testing.T) {
	r := newFrameRing(1024)
	sealed := [][]byte{
		[]byte("sealed-frame-one"),
		[]byte("sealed-frame-two"),
		[]byte("sealed-frame-three"),
	}
	for _, f := range sealed {
		r.Add(append([]byte(nil), f...))
	}

	got := r.Frames()
	if len(got) != len(sealed) {
		t.Fatalf("replayed %d frames, want %d", len(got), len(sealed))
	}
	for i := range sealed {
		if !bytes.Equal(got[i], sealed[i]) {
			t.Fatalf("frame %d came back as %q, want %q", i, got[i], sealed[i])
		}
	}
}

func TestFrameRingNeverExceedsItsCap(t *testing.T) {
	const maxBytes = 64
	rng := rand.New(rand.NewSource(1))
	r := newFrameRing(maxBytes)

	var want [][]byte
	wantBytes := 0
	for i := 0; i < 500; i++ {
		p := make([]byte, 1+rng.Intn(40))
		for j := range p {
			p[j] = byte('a' + rng.Intn(26))
		}
		r.Add(append([]byte(nil), p...))

		// The obvious slow model: append, then drop whole frames from the
		// front until it fits.
		if len(p) >= maxBytes {
			want = [][]byte{p}
			wantBytes = len(p)
		} else {
			want = append(want, p)
			wantBytes += len(p)
			for wantBytes > maxBytes && len(want) > 0 {
				wantBytes -= len(want[0])
				want = want[1:]
			}
		}

		if r.Len() != wantBytes {
			t.Fatalf("iteration %d: Len = %d, want %d", i, r.Len(), wantBytes)
		}
		got := r.Frames()
		if len(got) != len(want) {
			t.Fatalf("iteration %d: %d frames, want %d", i, len(got), len(want))
		}
		for j := range want {
			if !bytes.Equal(got[j], want[j]) {
				t.Fatalf("iteration %d frame %d differs", i, j)
			}
		}
	}
}

// Frames returns a copy of the slice, so a caller iterating it cannot be
// tripped up by later writes.
func TestFrameRingFramesIsASnapshot(t *testing.T) {
	r := newFrameRing(64)
	r.Add([]byte("first"))
	snapshot := r.Frames()

	for i := 0; i < 40; i++ {
		r.Add([]byte("more"))
	}
	if len(snapshot) != 1 || string(snapshot[0]) != "first" {
		t.Fatal("an earlier snapshot changed underneath its caller")
	}
}
