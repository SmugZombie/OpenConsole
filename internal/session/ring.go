package session

// frameRing keeps the most recent terminal output for replay to a guest that
// joins mid-session, so it sees a screen rather than a blank rectangle.
//
// It stores whole payloads rather than a byte stream, and replays them as the
// separate frames they arrived as. That distinction is not cosmetic: with
// end-to-end encryption each frame is independently sealed, and concatenating
// two of them produces something that will never decrypt. A byte-oriented
// buffer works perfectly for plaintext and silently breaks every encrypted
// session, which is exactly the kind of bug that reaches production.
//
// It is not safe for concurrent use; the bridge holds its mutex around access.
type frameRing struct {
	// maxBytes caps the total payload retained, not the number of frames: a
	// terminal emits both single keystrokes and screen-sized bursts.
	maxBytes int
	bytes    int
	frames   [][]byte
}

func newFrameRing(maxBytes int) *frameRing {
	return &frameRing{maxBytes: maxBytes}
}

// Add appends a payload, dropping whole frames from the front until the total
// fits. p is retained, so callers must not reuse it.
func (r *frameRing) Add(p []byte) {
	if r.maxBytes <= 0 || len(p) == 0 {
		return
	}

	// A single frame larger than the whole buffer replaces everything. It
	// cannot be trimmed: half of a sealed frame is not a frame.
	if len(p) >= r.maxBytes {
		r.frames = [][]byte{p}
		r.bytes = len(p)
		return
	}

	r.frames = append(r.frames, p)
	r.bytes += len(p)

	for r.bytes > r.maxBytes && len(r.frames) > 0 {
		r.bytes -= len(r.frames[0])
		r.frames[0] = nil // let the payload be collected
		r.frames = r.frames[1:]
	}
}

// Frames returns the retained payloads, oldest first. The slice is a copy; the
// payloads are not, and must not be modified.
func (r *frameRing) Frames() [][]byte {
	if len(r.frames) == 0 {
		return nil
	}
	out := make([][]byte, len(r.frames))
	copy(out, r.frames)
	return out
}

// Len reports how many bytes are retained.
func (r *frameRing) Len() int { return r.bytes }

// Count reports how many frames are retained.
func (r *frameRing) Count() int { return len(r.frames) }
