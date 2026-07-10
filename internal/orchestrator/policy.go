package orchestrator

import (
	"time"
)

// FixedCountPolicy is the reference RetryPolicy implementation
// (whitepaper §20: "A new retry strategy: RetryPolicy").
//
// Binding contract (core.RetryPolicy's doc comment; whitepaper §6/§7.1):
// count MUST be the persisted steps.attempt_count read from the store, never
// a loop-local counter — loop.go's dispatchAndResolve re-reads
// LoadFrontier().AttemptCount before every call to Next.
//
// v1.0 role change: the AUTHORITATIVE worker-side DLQ decision is made by
// StateStore.RecordFailure (TX3), which compares the persisted count against
// the workflow's retry_limit inside the same transaction as the attempt's
// failure verdict (whitepaper §7.1's "one blade" — verdict and DLQ cannot be
// split by a crash). The loop consults RecordFailure's returned
// FailureOutcome.DLQed for routing, NOT this policy's toDLQ return value.
// FixedCountPolicy.Next is therefore only ever used by the loop for its
// Delay return (the 5s cooldown, whitepaper §6) — MaxRetries is kept here
// only because the interface's shape (count, err) → (delay, toDLQ) is a
// stable extension point (whitepaper §20) for future strategies (e.g.
// exponential backoff) that might want the threshold too; it is dead
// weight for the current fixed-delay policy's actual use in the loop, but
// removing it would narrow the interface's only reference implementation
// for no present benefit.
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

// Next implements core.RetryPolicy. count is the persisted
// steps.attempt_count (the binding contract above) — not a loop-local index.
func (p *FixedCountPolicy) Next(count int, _ error) (time.Duration, bool) {
	if count >= p.MaxRetries {
		return 0, true // retries exhausted → DLQ (informational; the loop
		// does not act on this — see the type doc comment)
	}
	return p.Delay, false
}
