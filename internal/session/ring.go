package session

// ringBuffer keeps the most recent size bytes written to it.
//
// It backs terminal scrollback replay for joining guests: writes happen on
// every chunk of terminal output, reads only when someone joins, so it favours
// cheap appends and copies on read.
//
// It is not safe for concurrent use; the bridge holds its mutex around access.
type ringBuffer struct {
	buf   []byte // allocated lazily, always len == size once allocated
	size  int
	start int // index of the oldest byte
	n     int // bytes currently held, never more than size
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{size: size}
}

// Write appends p, discarding the oldest bytes that no longer fit.
func (r *ringBuffer) Write(p []byte) {
	if r.size <= 0 || len(p) == 0 {
		return
	}
	if r.buf == nil {
		// Allocated on first write so an idle session costs nothing.
		r.buf = make([]byte, r.size)
	}

	// A write at least as large as the buffer replaces it entirely; only its
	// tail can survive.
	if len(p) >= r.size {
		copy(r.buf, p[len(p)-r.size:])
		r.start, r.n = 0, r.size
		return
	}

	end := (r.start + r.n) % r.size
	c := copy(r.buf[end:], p)
	if c < len(p) {
		copy(r.buf, p[c:]) // wrapped
	}

	if r.n+len(p) > r.size {
		// The oldest bytes were overwritten; slide start past them.
		r.start = (r.start + (r.n + len(p) - r.size)) % r.size
		r.n = r.size
	} else {
		r.n += len(p)
	}
}

// Bytes returns a copy of the contents, oldest first.
func (r *ringBuffer) Bytes() []byte {
	if r.n == 0 {
		return nil
	}
	out := make([]byte, 0, r.n)
	if end := r.start + r.n; end <= r.size {
		out = append(out, r.buf[r.start:end]...)
	} else {
		out = append(out, r.buf[r.start:]...)
		out = append(out, r.buf[:end-r.size]...)
	}
	return out
}

// Len reports how many bytes are retained.
func (r *ringBuffer) Len() int { return r.n }
