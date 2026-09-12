// Package rawfailure is milestone theta's second test group: the two ways a
// raw worker can fail that only raw mode can produce.
//
// SPEC.md 9.6 gives raw mode its own success and failure rules, and both
// failures here are invisible in envelope mode:
//
//   - a non-2xx, where "the truncated body is stored as error text" - envelope
//     mode has a `status` field to report failure and does not need the HTTP
//     code to carry the verdict;
//   - a 2xx whose body is not a valid JSON document, which 9.6 makes
//     invalid_response - a rule that exists only because a raw worker's whole
//     body becomes the output.
//
// They are one group in the sense of CLAUDE.md 5.5 - one environment, brought
// up once and torn down with a volume wipe - and two runs inside it, because
// one workflow cannot fail in two ways at once. The two runs do not interact:
// neither reads global coordination state, which is what 5.5.3 reserves a
// separate group for.
package rawfailure

import (
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/aaronwu001/piton/test/theta/harness"
)

var (
	// The non-2xx run: one raw step against an endpoint answering HTTP 500.
	non2xxRun    string
	non2xxStatus string

	// The not-JSON run: one raw step against an endpoint answering 200 with
	// prose.
	notJSONRun    string
	notJSONStatus string

	// maxAttempts is step_max_attempts as the two workflow files declare it,
	// read from the files so that the budget and the assertion about it cannot
	// drift apart (SPEC.md 12.2).
	maxAttempts int
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		fmt.Println("theta/rawfailure: skipped in -short mode; this group needs docker compose")
		os.Exit(0)
	}

	var err error
	if maxAttempts, err = harness.WorkflowSetting(harness.WorkflowNon2xx, "step_max_attempts"); err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawfailure:", err)
		os.Exit(1)
	}
	other, err := harness.WorkflowSetting(harness.WorkflowNotJSON, "step_max_attempts")
	if err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawfailure:", err)
		os.Exit(1)
	}
	if other != maxAttempts {
		// Not a SPEC rule - a property of this group's fixture. The tests below
		// assert one budget against both runs, so the two files must agree.
		fmt.Fprintf(os.Stderr, "theta/rawfailure: the two workflow files declare different "+
			"step_max_attempts (%d and %d); this group's assertions assume one budget\n",
			maxAttempts, other)
		os.Exit(1)
	}

	if err = harness.Up(); err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawfailure: the environment did not come up:", err)
		os.Exit(1)
	}

	code := run(m)
	if err := harness.Down(); err != nil {
		fmt.Fprintln(os.Stderr, "theta/rawfailure: teardown failed:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(m *testing.M) int {
	var err error
	if _, non2xxRun, non2xxStatus, err = harness.Seed(harness.WorkflowNon2xx); err != nil {
		return fail("the non-2xx fixture could not be created", err)
	}
	if _, notJSONRun, notJSONStatus, err = harness.Seed(harness.WorkflowNotJSON); err != nil {
		return fail("the not-JSON fixture could not be created", err)
	}
	fmt.Printf("theta/rawfailure: non2xx run=%s final=%s;  notJSON run=%s final=%s\n",
		non2xxRun, non2xxStatus, notJSONRun, notJSONStatus)
	return m.Run()
}

func fail(what string, err error) int {
	fmt.Fprintln(os.Stderr, "theta/rawfailure:", what+":", err)
	fmt.Fprintln(os.Stderr, "\n--- orchestrator logs ---")
	fmt.Fprintln(os.Stderr, harness.OrchestratorLogs(60))
	return 1
}
