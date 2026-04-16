// Package ratelimit provides a global sliding-window rate limiter for Anthropic API calls.
// All callers (single-agent and all multi-agent subagents/goroutines) share the same limiter,
// so the combined call rate never exceeds the configured limit.
package ratelimit

import (
	"fmt"
	"sync"
	"time"
)

// limiter is the single shared instance used by the whole process.
var limiter = &slidingWindowLimiter{
	maxCalls: 25,
	window:   60 * time.Second,
}

// slidingWindowLimiter tracks the timestamps of recent API calls.
// Before each call, Wait() blocks until a slot is available within the rolling window.
type slidingWindowLimiter struct {
	mu        sync.Mutex
	callTimes []time.Time
	maxCalls  int
	window    time.Duration
}

// Wait blocks until the caller is allowed to make an API call, then records the call time.
// It prints a message when it has to wait so the user knows what is happening.
func Wait() {
	limiter.wait()
}

// SetLimit lets callers override the default max calls per minute (useful for testing or higher tiers).
func SetLimit(maxCallsPerMinute int) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	limiter.maxCalls = maxCallsPerMinute
}

func (r *slidingWindowLimiter) wait() {
	for {
		r.mu.Lock()

		now := time.Now()
		windowStart := now.Add(-r.window)

		// Evict timestamps that have fallen outside the rolling window.
		j := 0
		for _, t := range r.callTimes {
			if t.After(windowStart) {
				r.callTimes[j] = t
				j++
			}
		}
		r.callTimes = r.callTimes[:j]

		if len(r.callTimes) < r.maxCalls {
			// Slot available — record this call and proceed immediately.
			r.callTimes = append(r.callTimes, now)
			r.mu.Unlock()
			return
		}

		// All slots used. Wait until the oldest call rolls out of the window.
		oldest := r.callTimes[0]
		waitUntil := oldest.Add(r.window).Add(200 * time.Millisecond) // small buffer
		waitDuration := time.Until(waitUntil)
		used := len(r.callTimes)
		max := r.maxCalls
		r.mu.Unlock()

		if waitDuration > 0 {
			fmt.Printf("\n  [RateLimiter] %d/%d calls used in last 60s — waiting %.0fs for next slot...\n\n",
				used, max, waitDuration.Seconds())
			time.Sleep(waitDuration)
		}
		// Loop again to re-check after sleeping (another goroutine may have taken the slot).
	}
}
