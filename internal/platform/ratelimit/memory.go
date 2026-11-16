package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// Memory is an in-process token-bucket Limiter. Buckets idle for longer than IdleTTL are dropped by Sweep
// (or by the janitor started with StartJanitor), so memory stays bounded by the number of active keys.
type Memory struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	idleTTL time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
}

// MemoryOptions tunes NewMemory.
type MemoryOptions struct {
	IdleTTL time.Duration    // default 10 minutes
	Now     func() time.Time // clock, for tests
}

// NewMemory returns an empty in-memory limiter.
func NewMemory(o MemoryOptions) *Memory {
	if o.IdleTTL <= 0 {
		o.IdleTTL = 10 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Memory{buckets: make(map[string]*bucket), now: o.Now, idleTTL: o.IdleTTL}
}

// Allow takes one token from key's bucket. Different rates must use different key prefixes (the HTTP
// middleware prefixes keys with the profile name), because a bucket does not remember its rate.
func (m *Memory) Allow(_ context.Context, key string, rate Rate) (Decision, error) {
	burst := float64(rate.burst())
	perSec := rate.perSecond()
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		m.buckets[key] = b
	} else if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(burst, b.tokens+elapsed*perSec)
		b.last = now
	}
	d := Decision{Limit: int(burst)}
	if b.tokens >= 1 {
		b.tokens--
		d.Allowed = true
		d.Remaining = int(b.tokens)
		return d, nil
	}
	if perSec > 0 {
		d.RetryAfter = time.Duration((1 - b.tokens) / perSec * float64(time.Second))
	} else {
		d.RetryAfter = time.Hour
	}
	return d, nil
}

// Sweep drops buckets that have been idle for at least IdleTTL and returns how many it removed. An idle
// bucket has refilled anyway, so dropping it does not change any decision as long as IdleTTL covers the
// slowest refill (Burst / rate).
func (m *Memory) Sweep() int {
	cutoff := m.now().Add(-m.idleTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, b := range m.buckets {
		if !b.last.After(cutoff) {
			delete(m.buckets, k)
			n++
		}
	}
	return n
}

// Len returns the number of live buckets.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}

// StartJanitor runs Sweep every interval until ctx is done.
func (m *Memory) StartJanitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Sweep()
			}
		}
	}()
}
