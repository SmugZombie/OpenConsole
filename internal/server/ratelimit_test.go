package server

import (
	"sync"
	"testing"
	"time"
)

// testClock is a hand-wound clock, so the limiter can be tested without
// sleeping and without flakiness.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestRateLimiterAllowsBurstThenRefuses(t *testing.T) {
	clock := newTestClock()
	l := newRateLimiter(60, 5, clock.Now) // one per second, five at once

	for i := 0; i < 5; i++ {
		if ok, _ := l.allow("1.2.3.4"); !ok {
			t.Fatalf("request %d in the burst was refused", i+1)
		}
	}

	ok, retry := l.allow("1.2.3.4")
	if ok {
		t.Fatal("the sixth request was allowed; the burst is not a ceiling")
	}
	if retry <= 0 {
		t.Fatal("a refusal should say when to try again")
	}
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	clock := newTestClock()
	l := newRateLimiter(60, 2, clock.Now)

	l.allow("a")
	l.allow("a")
	if ok, _ := l.allow("a"); ok {
		t.Fatal("the bucket did not empty")
	}

	clock.Advance(time.Second)
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("a token should have refilled after a second")
	}

	// And refill is capped at the burst, not accumulated forever.
	clock.Advance(time.Hour)
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("request %d after a long idle was refused", i+1)
		}
	}
	if ok, _ := l.allow("a"); ok {
		t.Fatal("an hour idle granted more than the burst")
	}
}

// One noisy source must not consume anybody else's allowance.
func TestRateLimiterIsPerSource(t *testing.T) {
	clock := newTestClock()
	l := newRateLimiter(60, 2, clock.Now)

	l.allow("noisy")
	l.allow("noisy")
	if ok, _ := l.allow("noisy"); ok {
		t.Fatal("the noisy source was not limited")
	}

	if ok, _ := l.allow("quiet"); !ok {
		t.Fatal("a different source was refused because of the noisy one")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	l := newRateLimiter(0, 0, newTestClock().Now)
	if l.enabled() {
		t.Fatal("a zero rate should disable limiting")
	}
	for i := 0; i < 1000; i++ {
		if ok, _ := l.allow("anyone"); !ok {
			t.Fatal("a disabled limiter refused a request")
		}
	}
}

// The bucket map is itself a memory-exhaustion risk, since the key is chosen by
// whoever is connecting.
func TestRateLimiterSweepsIdleBuckets(t *testing.T) {
	clock := newTestClock()
	l := newRateLimiter(60, 1, clock.Now)

	for i := 0; i < 100; i++ {
		l.allow(string(rune('a'+i%26)) + itoaTest(i))
	}
	if l.len() == 0 {
		t.Fatal("nothing was tracked")
	}

	clock.Advance(time.Hour)
	if n := l.sweep(); n == 0 {
		t.Fatal("idle buckets were not swept")
	}
	if l.len() != 0 {
		t.Fatalf("%d buckets survived the sweep", l.len())
	}
}

func TestRateLimiterCapsTrackedSources(t *testing.T) {
	clock := newTestClock()
	l := newRateLimiter(60, 1, clock.Now)

	// A flood of distinct sources, as a spoofed or rotating attacker produces.
	for i := 0; i < maxBuckets+500; i++ {
		l.allow(itoaTest(i))
	}
	if l.len() > maxBuckets {
		t.Fatalf("tracked %d sources, cap is %d", l.len(), maxBuckets)
	}
	// Refusing is the safe failure here; admitting everything would defeat the
	// limiter precisely when it is needed.
	if ok, _ := l.allow("brand-new-source"); ok {
		t.Fatal("a new source was admitted after the table filled")
	}
}

func TestRateLimiterIsConcurrencySafe(t *testing.T) {
	l := newRateLimiter(6000, 100, nil)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.allow(itoaTest(i))
			}
		}(i)
	}
	wg.Wait()
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
