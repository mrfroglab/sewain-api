// Package ratelimit provides token-bucket rate limiting for the API (ROADMAP F5.2).
//
// The Limiter interface lets the in-memory implementation be swapped for a shared one (Redis, Postgres)
// later. The in-memory Memory limiter keeps its buckets per process, so with N API instances the effective
// limit is N times the configured one.
package ratelimit

import (
	"context"
	"time"
)

// Rate is a token-bucket configuration: Burst tokens at most, refilled at Limit tokens per Per.
type Rate struct {
	Limit int
	Per   time.Duration
	Burst int
}

// perSecond returns the refill rate in tokens per second.
func (r Rate) perSecond() float64 {
	if r.Per <= 0 || r.Limit <= 0 {
		return 0
	}
	return float64(r.Limit) / r.Per.Seconds()
}

func (r Rate) burst() int {
	if r.Burst > 0 {
		return r.Burst
	}
	if r.Limit > 0 {
		return r.Limit
	}
	return 1
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed    bool
	Limit      int           // burst size, for RateLimit-Limit style headers
	Remaining  int           // whole tokens left after this request
	RetryAfter time.Duration // when denied: time until one token is available
}

// Limiter decides whether the request identified by key may proceed under rate.
type Limiter interface {
	Allow(ctx context.Context, key string, rate Rate) (Decision, error)
}
