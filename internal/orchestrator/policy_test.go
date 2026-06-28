package orchestrator_test

import (
	"errors"
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
		delay, toDLQ := p.Next(tc.attempt, dummy)
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

	_, toDLQ1 := p.Next(1, errors.New("transient"))
	_, toDLQ2 := p.Next(1, errors.New("fatal"))
	if toDLQ1 != toDLQ2 {
		t.Error("different errors produced different decisions — policy must be error-agnostic")
	}
	t.Log("PASS — error argument has no effect on retry decision")
}
