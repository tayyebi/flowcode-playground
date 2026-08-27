package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterBurstThenRefill(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(60, 3, time.Minute) // 1/sec, burst 3
	rl.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := rl.allow("a"); !ok {
			t.Fatalf("request %d denied inside the burst", i+1)
		}
	}
	ok, retry := rl.allow("a")
	if ok {
		t.Fatal("4th request allowed, want denied")
	}
	if retry <= 0 {
		t.Errorf("Retry-After = %s, want a positive duration", retry)
	}

	// One token per second, so a second later exactly one more gets through.
	now = now.Add(time.Second)
	if ok, _ := rl.allow("a"); !ok {
		t.Error("request denied after the bucket should have refilled")
	}
	if ok, _ := rl.allow("a"); ok {
		t.Error("bucket refilled by more than the elapsed time allows")
	}
}

func TestRateLimiterIsPerKey(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(60, 1, time.Minute)
	rl.now = func() time.Time { return now }

	if ok, _ := rl.allow("a"); !ok {
		t.Fatal("first request for a denied")
	}
	if ok, _ := rl.allow("a"); ok {
		t.Fatal("second request for a allowed")
	}
	if ok, _ := rl.allow("b"); !ok {
		t.Error("b was limited by a's usage")
	}
}

func TestRateLimiterSweepEvictsIdle(t *testing.T) {
	now := time.Now()
	rl := newRateLimiter(60, 1, time.Minute)
	rl.now = func() time.Time { return now }

	rl.allow("a")
	now = now.Add(30 * time.Second)
	rl.allow("b")

	now = now.Add(31 * time.Second) // a is now 61s idle, b is 31s
	rl.sweep()

	if _, ok := rl.buckets["a"]; ok {
		t.Error("idle bucket a survived the sweep")
	}
	if _, ok := rl.buckets["b"]; !ok {
		t.Error("recently used bucket b was evicted")
	}
}

func TestRateLimiterMiddlewareSends429(t *testing.T) {
	rl := newRateLimiter(60, 1, time.Minute)
	handler := rl.middleware(false, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/run", nil)
		r.RemoteAddr = "192.0.2.10:5555"
		return r
	}

	first := httptest.NewRecorder()
	handler(first, newReq())
	if first.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	handler(second, newReq())
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("429 response is missing Retry-After")
	}
}

func TestClientKeyIgnoresForwardedHeaderWhenUntrusted(t *testing.T) {
	// A forgeable header must not create a fresh bucket per request; that would
	// look like rate limiting while providing none.
	r := httptest.NewRequest(http.MethodPost, "/api/run", nil)
	r.RemoteAddr = "192.0.2.10:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.1")

	if got := clientKey(r, false); got != "192.0.2.10" {
		t.Errorf("clientKey(untrusted) = %q, want the socket address", got)
	}
	if got := clientKey(r, true); got != "203.0.113.1" {
		t.Errorf("clientKey(trusted) = %q, want the forwarded address", got)
	}
}

func TestClientKeyUsesFirstForwardedHop(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/run", nil)
	r.RemoteAddr = "192.0.2.10:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.1, 198.51.100.7")

	if got := clientKey(r, true); got != "203.0.113.1" {
		t.Errorf("clientKey = %q, want the client-most hop", got)
	}
}
