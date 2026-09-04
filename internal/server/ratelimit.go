package server

import (
	"sync"
	"time"
)

// A token bucket per source address.
//
// Written out rather than pulled from a dependency: it is forty lines, the
// project has no other need for the module, and a limiter whose behaviour you
// can read is easier to trust than one you have to look up.
//
// The buckets themselves are a memory-exhaustion risk — the very thing this
// exists to prevent — because a source address is attacker-chosen and IPv6 has
// plenty of them. So idle buckets are swept, and the map is capped.

// maxBuckets bounds how many sources are tracked at once.
//
// Past this, new sources are refused rather than admitted: under a flood from
// spoofed or rotating addresses, refusing is the safe failure. Legitimate
// traffic from a handful of addresses never comes close.
const maxBuckets = 8192

// bucket is one source's allowance.
type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter allows a burst, then a steady rate, per source.
type rateLimiter struct {
	// rate is tokens added per second; burst is the ceiling.
	rate  float64
	burst float64
	now   func() time.Time
	// idle is how long a bucket may go untouched before it is swept.
	idle time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

// newRateLimiter builds a limiter allowing perMinute requests per source in a
// steady state, with burst available at once. A non-positive rate disables
// limiting entirely.
func newRateLimiter(perMinute, burst int, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	if burst < 1 {
		burst = 1
	}
	return &rateLimiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		now:     now,
		idle:    10 * time.Minute,
		buckets: make(map[string]*bucket),
	}
}

// enabled reports whether the limiter does anything.
func (l *rateLimiter) enabled() bool { return l != nil && l.rate > 0 }

// allow takes a token for key, reporting whether one was available and, if
// not, how long until one is.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	if !l.enabled() {
		return true, 0
	}

	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			// Sweep before giving up: the map may be full of stale entries.
			l.sweepLocked(now)
		}
		if len(l.buckets) >= maxBuckets {
			return false, time.Minute
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	// Refill for the time that has passed, up to the burst ceiling.
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// How long until the next whole token.
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// sweepLocked drops buckets that have been idle long enough to have refilled
// anyway, so forgetting them changes nothing.
func (l *rateLimiter) sweepLocked(now time.Time) int {
	n := 0
	for key, b := range l.buckets {
		if now.Sub(b.last) > l.idle {
			delete(l.buckets, key)
			n++
		}
	}
	return n
}

// sweep is sweepLocked with the lock taken.
func (l *rateLimiter) sweep() int {
	if !l.enabled() {
		return 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sweepLocked(now)
}

// len reports how many sources are tracked, for tests.
func (l *rateLimiter) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
