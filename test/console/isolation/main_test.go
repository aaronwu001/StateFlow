// Package isolation asserts that breaking one run does not break another.
//
// WHY THIS IS ITS OWN GROUP
//
//	CLAUDE.md 5.5 makes one package one environment. This group creates runs
//	while others are still in flight and leaves some of them dead on purpose,
//	so a suite that counted runs — which test/console/readapi does — would see
//	a database no single test put into that condition. That is 5.5.3's reason
//	for a separate group, applied to a fixture rather than to coordination
//	state.
//
// WHAT IT IS ACTUALLY TESTING
//
//	Nothing in SPEC.md mentions a pause switch: it belongs to the demo
//	environment, not to Piton. What the assertions cite is what the pause
//	PRODUCES — SPEC.md 5.3's `timeout`, SPEC.md 12.2's march to DLQ, and the
//	fact that a run is the unit all of that applies to. The switch is the
//	mechanism, in the same way that a params-driven worker is the mechanism in
//	milestone gamma.
package isolation

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/aaronwu001/piton/test/console/harness"
)

var workflowID string

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("console/isolation: skipped in -short mode; this group needs docker compose")
		os.Exit(0)
	}

	if err := harness.Up(); err != nil {
		fmt.Fprintln(os.Stderr, "console/isolation: the environment did not come up:", err)
		os.Exit(1)
	}

	code := run(m)
	if err := harness.Down(); err != nil {
		fmt.Fprintln(os.Stderr, "console/isolation: teardown failed:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(m *testing.M) int {
	if err := harness.WaitHealthy(harness.HealthTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "console/isolation:", err)
		return 1
	}
	var err error
	if workflowID, err = harness.CreateWorkflow(); err != nil {
		fmt.Fprintln(os.Stderr, "console/isolation:", err)
		return 1
	}
	return m.Run()
}
