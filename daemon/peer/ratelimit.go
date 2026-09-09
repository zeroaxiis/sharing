package peer

import (
	"sync"
	"time"
)

// sweepThreshold is how many distinct source addresses the limiter tolerates
// before it prunes expired ones wholesale. Without a sweep, a host that dials
// from a fresh source address every time would grow the map forever.
const sweepThreshold = 1024

// rateLimiter is a sliding-window counter keyed by source address.
//
// It is the only thing standing between a six-digit pairing code and a LAN
// attacker with a script: 10^6 codes at five attempts a minute is roughly four
// years of guessing per device, and every attempt puts a prompt in front of a
// human who will notice. The window slides rather than resetting on a fixed
// boundary, so an attacker cannot line up bursts either side of a reset and get
// double the budget.
//
// A denied attempt is deliberately NOT recorded. Recording it would let a
// hammering attacker keep their own window permanently full, which sounds
// harmless until a legitimate device behind the same NAT wants to pair and
// finds the window never drains.
type rateLimiter struct {
	limit  int
	window time.Duration

	// now is injectable so the tests do not have to sleep through a minute.
	now func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: window,
		now:    time.Now,
		hits:   make(map[string][]time.Time),
	}
}

// allow records an attempt from key and reports whether it is within budget.
func (r *rateLimiter) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	cutoff := now.Add(-r.window)

	kept := prune(r.hits[key], cutoff)
	if len(kept) >= r.limit {
		r.hits[key] = kept
		return false
	}

	r.hits[key] = append(kept, now)
	if len(r.hits) > sweepThreshold {
		r.sweepLocked(cutoff, key)
	}
	return true
}

// remaining reports how many attempts key has left in the current window. It
// records nothing; it exists for logging and for the tests.
func (r *rateLimiter) remaining(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	kept := prune(r.hits[key], r.now().Add(-r.window))
	r.hits[key] = kept
	if len(kept) >= r.limit {
		return 0
	}
	return r.limit - len(kept)
}

// sweepLocked drops keys whose whole window has expired. keep is never removed,
// because allow has just written to it.
func (r *rateLimiter) sweepLocked(cutoff time.Time, keep string) {
	for k, times := range r.hits {
		if k == keep {
			continue
		}
		if kept := prune(times, cutoff); len(kept) == 0 {
			delete(r.hits, k)
		} else {
			r.hits[k] = kept
		}
	}
}

// prune returns the timestamps still inside the window. It allocates a fresh
// slice rather than filtering in place: the caller may be holding the old slice
// (the tests do), and silently rewriting its backing array would be a trap.
func prune(times []time.Time, cutoff time.Time) []time.Time {
	if len(times) == 0 {
		return nil
	}
	out := make([]time.Time, 0, len(times))
	for _, t := range times {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}
