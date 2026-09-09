package peer

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// fakeClock lets the limiter be tested at full speed instead of sleeping
// through its window.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestLimiter(limit int, window time.Duration) (*rateLimiter, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	r := newRateLimiter(limit, window)
	r.now = clock.Now
	return r, clock
}

func TestRateLimiterAllowsUpToTheLimit(t *testing.T) {
	t.Parallel()
	r, _ := newTestLimiter(protocol.PeerPairAttemptsPerWindow, protocol.PeerPairRateWindow)

	for i := 1; i <= protocol.PeerPairAttemptsPerWindow; i++ {
		if !r.allow("192.168.1.10") {
			t.Fatalf("attempt %d was refused, want allowed", i)
		}
	}
	if r.allow("192.168.1.10") {
		t.Fatal("attempt beyond the limit was allowed")
	}
	if got := r.remaining("192.168.1.10"); got != 0 {
		t.Fatalf("remaining = %d, want 0", got)
	}
}

func TestRateLimiterWindowSlides(t *testing.T) {
	t.Parallel()
	r, clock := newTestLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !r.allow("10.0.0.1") {
			t.Fatalf("attempt %d refused", i+1)
		}
		clock.advance(10 * time.Second)
	}
	if r.allow("10.0.0.1") {
		t.Fatal("fourth attempt inside the window was allowed")
	}

	// The first attempt is 30s old; the window is 60s. Nothing has expired yet.
	clock.advance(25 * time.Second)
	if r.allow("10.0.0.1") {
		t.Fatal("an attempt was allowed while the window was still full")
	}

	// Now the oldest one falls out and exactly one slot opens.
	clock.advance(10 * time.Second)
	if !r.allow("10.0.0.1") {
		t.Fatal("no slot opened after the oldest attempt expired")
	}
	if r.allow("10.0.0.1") {
		t.Fatal("more than one slot opened")
	}
}

// A blocked source must not extend its own block by hammering: a refused
// attempt is not recorded, so the window still drains on time.
func TestRateLimiterDoesNotRecordRefusedAttempts(t *testing.T) {
	t.Parallel()
	r, clock := newTestLimiter(2, time.Minute)

	r.allow("10.0.0.2")
	r.allow("10.0.0.2")
	for i := 0; i < 50; i++ {
		if r.allow("10.0.0.2") {
			t.Fatal("a refused source was let through")
		}
		clock.advance(time.Second)
	}
	// 50s of hammering later the two real attempts are 60s and 59s old.
	clock.advance(11 * time.Second)
	if !r.allow("10.0.0.2") {
		t.Fatal("the window never drained")
	}
}

// The limit is per source, so one hostile machine cannot lock out the LAN.
func TestRateLimiterIsPerSource(t *testing.T) {
	t.Parallel()
	r, _ := newTestLimiter(2, time.Minute)

	r.allow("10.0.0.3")
	r.allow("10.0.0.3")
	if r.allow("10.0.0.3") {
		t.Fatal("the noisy source was not limited")
	}
	if !r.allow("10.0.0.4") {
		t.Fatal("a different source was limited by its neighbour")
	}
}

func TestRateLimiterSweepsExpiredSources(t *testing.T) {
	t.Parallel()
	r, clock := newTestLimiter(2, time.Minute)

	for i := 0; i < sweepThreshold+10; i++ {
		r.allow(fmt.Sprintf("10.1.%d.%d", i/256, i%256))
	}
	clock.advance(2 * time.Minute)
	r.allow("10.9.9.9")

	r.mu.Lock()
	size := len(r.hits)
	r.mu.Unlock()
	if size > 2 {
		t.Fatalf("limiter still tracks %d expired sources, want them swept", size)
	}
}

func TestRateLimiterIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	r := newRateLimiter(100, time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.allow(fmt.Sprintf("10.2.0.%d", n%4))
				r.remaining("10.2.0.0")
			}
		}(i)
	}
	wg.Wait()
}
