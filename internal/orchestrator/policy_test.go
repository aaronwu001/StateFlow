package orchestrator_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aaronwu000/stateflow/internal/orchestrator"
)

func TestFixedCountPolicy_Defaults(t *testing.T) {
	p := orchestrator.DefaultRetryPolicy()
	if p.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", p.MaxRetries)
	}
	if p.Delay != 5*time.Second {
		t.Errorf("Delay = %v, want 5s", p.Delay)
	}
}

// TestFixedCountPolicy_RetryThenDLQ verifies the boundary:
// attempts 1 and 2 → retry; attempt 3 → DLQ (MaxRetries=3).
func TestFixedCountPolicy_RetryThenDLQ(t *testing.T) {
	p := &orchestrator.FixedCountPolicy{MaxRetries: 3, Delay: 10 * time.Millisecond}
	dummy := errors.New("worker failed")

	cases := []struct {
		attempt  int
		wantDLQ  bool
		wantZero bool // delay is 0 when toDLQ
	}{
		{1, false, false},
		{2, false, false},
		{3, true, true},
	}

	for _, tc := range cases {
		delay, toDLQ := p.Next(tc.attempt, dummy, nil)
		if toDLQ != tc.wantDLQ {
			t.Errorf("attempt %d: toDLQ = %v, want %v", tc.attempt, toDLQ, tc.wantDLQ)
		}
		if tc.wantZero && delay != 0 {
			t.Errorf("attempt %d: delay = %v, want 0 (toDLQ path)", tc.attempt, delay)
		}
		if !tc.wantDLQ && delay != p.Delay {
			t.Errorf("attempt %d: delay = %v, want %v", tc.attempt, delay, p.Delay)
		}
		t.Logf("PASS — attempt %d: toDLQ=%v delay=%v", tc.attempt, toDLQ, delay)
	}
}

// TestFixedCountPolicy_ErrIgnored verifies that the error argument has no
// effect on the decision — fixed policy decides only on attempt count.
func TestFixedCountPolicy_ErrIgnored(t *testing.T) {
	p := &orchestrator.FixedCountPolicy{MaxRetries: 2, Delay: time.Second}

	_, toDLQ1 := p.Next(1, errors.New("transient"), nil)
	_, toDLQ2 := p.Next(1, errors.New("fatal"), nil)
	if toDLQ1 != toDLQ2 {
		t.Error("different errors produced different decisions — policy must be error-agnostic")
	}
	t.Log("PASS — error argument has no effect on retry decision")
}

// intPtr is a small test helper — Go has no literal syntax for a pointer to
// an int constant.
func intPtr(n int) *int { return &n }

// TestFixedCountPolicy_RetryAfterSecondsFloor is the registry item #5
// ("LLM-aware rate limiting") contract test: effective delay =
// max(worker's reported retry_after_seconds, the policy's own system-default
// Delay) — a floor, never a ceiling, and never a below-default value.
// Covers the session's three mandatory cases:
//
//	(a) worker requests 30s, above the 5s system default → 30s is honored.
//	(b) worker requests 1s, below the 5s system default → the 5s floor
//	    still applies (1s does NOT shorten the delay).
//	(c) worker omits retry_after_seconds (nil) → unchanged 5s behavior
//	    (regression check: nil must reduce to today's fixed-delay policy).
func TestFixedCountPolicy_RetryAfterSecondsFloor(t *testing.T) {
	p := orchestrator.DefaultRetryPolicy() // MaxRetries=3, Delay=5s — the real system default
	dummy := errors.New("worker_reported")

	cases := []struct {
		name              string
		retryAfterSeconds *int
		wantDelay         time.Duration
	}{
		{"above_floor_30s_honored", intPtr(30), 30 * time.Second},
		{"below_floor_1s_still_5s", intPtr(1), 5 * time.Second},
		{"omitted_nil_unchanged_5s", nil, 5 * time.Second},
		{"exactly_floor_5s", intPtr(5), 5 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delay, toDLQ := p.Next(1, dummy, tc.retryAfterSeconds) // count=1, below MaxRetries=3
			if toDLQ {
				t.Fatalf("toDLQ = true, want false (count=1 is below MaxRetries=3)")
			}
			if delay != tc.wantDelay {
				t.Errorf("delay = %v, want %v", delay, tc.wantDelay)
			}
			display := "nil"
			if tc.retryAfterSeconds != nil {
				display = fmt.Sprintf("%d", *tc.retryAfterSeconds)
			}
			t.Logf("PASS — retryAfterSeconds=%s → delay=%v (want %v)", display, delay, tc.wantDelay)
		})
	}
}
