package panel

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a small in-memory throttle keyed by client IP. It tracks
// failures within a sliding window and blocks a key once it exceeds a
// threshold, which blunts online password/2FA guessing.
type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]*rlEntry

	max    int
	window time.Duration
	block  time.Duration
}

type rlEntry struct {
	failures     int
	first        time.Time
	blockedUntil time.Time
}

func newRateLimiter(max int, window, block time.Duration) *rateLimiter {
	return &rateLimiter{
		entries: make(map[string]*rlEntry),
		max:     max,
		window:  window,
		block:   block,
	}
}

// allow reports whether key may attempt a login, and if not, how long to wait.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e := l.entries[key]
	if e == nil {
		return true, 0
	}
	if now.Before(e.blockedUntil) {
		return false, e.blockedUntil.Sub(now)
	}
	if now.Sub(e.first) > l.window {
		delete(l.entries, key)
	}
	return true, 0
}

// fail records a failed attempt, blocking the key past the threshold.
func (l *rateLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e := l.entries[key]
	if e == nil || now.Sub(e.first) > l.window {
		e = &rlEntry{first: now}
		l.entries[key] = e
	}
	e.failures++
	if e.failures >= l.max {
		e.blockedUntil = now.Add(l.block)
		e.failures = 0
		e.first = now
	}
}

// reset clears the failure state for a key after a successful login.
func (l *rateLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// clientIP extracts the peer address, dropping the port.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
