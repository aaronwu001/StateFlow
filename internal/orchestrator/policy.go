package orchestrator

import (
	"time"
)

// FixedCountPolicy is the reference RetryPolicy implementation (DESIGN.md §8).
//
// Behaviour: allow up to MaxRetries total dispatch attempts per step.
// On each failure the loop calls Next(attemptNum, err):
//   - attemptNum < MaxRetries → return (Delay, toDLQ=false) — keep trying
//   - attemptNum >= MaxRetries → return (_, toDLQ=true) — route to DLQ
//
// Delay is fixed (no exponential backoff — deferred §9.2).
// retry_after from the worker response is accepted by the transport but
// ignored by this policy (also deferred §9.2).
type FixedCountPolicy struct {
	MaxRetries int
	Delay      time.Duration
}

// DefaultRetryPolicy returns the MVP default: 3 total attempts, 5s fixed delay.
func DefaultRetryPolicy() *FixedCountPolicy {
	return &FixedCountPolicy{
		MaxRetries: 3,
		Delay:      5 * time.Second,
	}
}

// Next implements core.RetryPolicy.
// attempt is the 1-based index of the attempt that just failed.
func (p *FixedCountPolicy) Next(attempt int, _ error) (time.Duration, bool) {
	if attempt >= p.MaxRetries {
		return 0, true // retries exhausted → DLQ
	}
	return p.Delay, false
}
