package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a per-client token bucket.
//
// Buckets are refilled lazily on access rather than by a ticker, so the cost is
// proportional to traffic rather than to the number of clients ever seen, and
// idle buckets are swept periodically so the map can't grow without bound from
// a stream of one-off addresses.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	rate  float64 // tokens per second
	burst float64
	idle  time.Duration

	// now is injectable so the tests don't have to sleep through real time.
	now func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute, burst int, idle time.Duration) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    float64(perMinute) / 60.0,
		burst:   float64(burst),
		idle:    idle,
		now:     time.Now,
	}
}

// allow consumes a token for key, reporting whether it was available and, if
// not, how long the caller should wait before the next one is.
func (rl *rateLimiter) allow(key string) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	b, ok := rl.buckets[key]
	if !ok {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	} else {
		b.tokens += now.Sub(b.last).Seconds() * rl.rate
		if b.tokens > rl.burst {
			b.tokens = rl.burst
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Round up: telling a client to retry in 0s when it must wait would just
	// produce another 429.
	wait := time.Duration((1-b.tokens)/rl.rate*float64(time.Second)) + time.Second
	return false, wait.Truncate(time.Second)
}

// sweep drops buckets that have been idle long enough to have fully refilled —
// forgetting them is then indistinguishable from keeping them.
func (rl *rateLimiter) sweep() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := rl.now().Add(-rl.idle)
	for k, b := range rl.buckets {
		if b.last.Before(cutoff) {
			delete(rl.buckets, k)
		}
	}
}

// run sweeps until ctx-like done channel closes.
func (rl *rateLimiter) run(done <-chan struct{}) {
	t := time.NewTicker(rl.idle)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			rl.sweep()
		}
	}
}

// middleware applies the limit to a handler.
func (rl *rateLimiter) middleware(trustProxy bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := clientKey(r, trustProxy)
		ok, retryAfter := rl.allow(key)
		if !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retryAfter.Seconds())))
			writeError(w, http.StatusTooManyRequests,
				"too many runs from this address; please slow down")
			return
		}
		next(w, r)
	}
}

// clientKey identifies the caller for rate limiting.
//
// X-Forwarded-For is only honoured when the operator has said a proxy is in
// front (TRUST_PROXY=1). Trusting it unconditionally would let any client
// forge the header and get a fresh bucket per request, which is worse than
// having no limit at all because it looks like one is working.
func clientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
