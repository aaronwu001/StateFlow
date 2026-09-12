package readapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aaronwu001/piton/test/console/harness"
)

// Every assertion names the SPEC.md rule it came from. CLAUDE.md 5.1 permits
// exactly one source, and API.md is authority on the wire only - where the two
// could disagree, SPEC.md wins, so the citations below are all to SPEC.md.

// ---------------------------------------------------------------------------
// The shapes API.md publishes
// ---------------------------------------------------------------------------

type runSummary struct {
	RunID               string  `json:"run_id"`
	WorkflowID          string  `json:"workflow_id"`
	Status              string  `json:"status"`
	PlannerAttemptCount int     `json:"planner_attempt_count"`
	ReplayCount         int     `json:"replay_count"`
	OwnerID             *string `json:"owner_id"`
	ClaimedAt           *string `json:"claimed_at"`
	CreatedAt           string  `json:"created_at"`
}

type runList struct {
	Runs       []runSummary `json:"runs"`
	NextCursor string       `json:"next_cursor"`
}

type dlqEntry struct {
	DLQID       string  `json:"dlq_id"`
	StepID      *string `json:"step_id"`
	Reason      string  `json:"reason"`
	ReplayRound int     `json:"replay_round"`
	ErrorText   string  `json:"error_text"`
	CreatedAt   string  `json:"created_at"`
}

type dlqList struct {
	RunID   string     `json:"run_id"`
	Entries []dlqEntry `json:"entries"`
}

type errBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func ids(runs []runSummary) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.RunID)
	}
	return out
}

func list(t *testing.T, query string) runList {
	t.Helper()
	var out runList
	if err := harness.GetJSON("/runs"+query, &out); err != nil {
		t.Fatalf("GET /runs%s: %v", query, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// GET /runs
// ---------------------------------------------------------------------------

// TestListReturnsEveryRunNewestFirst is SPEC.md 10.2's ordering rule: "newest
// first by created_at, with run_id breaking ties".
//
// The fixture created four runs in a known order, so the expected answer is
// that order reversed. Nothing else about the list can be trusted until this
// holds - a cursor into an unordered set is meaningless, which is why SPEC.md
// fixes the ordering rather than leaving it to the backend.
func TestListReturnsEveryRunNewestFirst(t *testing.T) {
	got := list(t, "?limit=200")
	if len(got.Runs) != len(createdOrder) {
		t.Fatalf("SPEC.md 10.2: GET /runs must list every run; expected %d, got %d (%v)",
			len(createdOrder), len(got.Runs), ids(got.Runs))
	}
	for i, want := range []string{runReplayDLQ, runPlanrDLQ, runWorkerDLQ, runDone} {
		if got.Runs[i].RunID != want {
			t.Errorf("SPEC.md 10.2: runs are newest first; position %d should be %s, got %s\n  full order: %v",
				i, want, got.Runs[i].RunID, ids(got.Runs))
		}
	}
	if got.NextCursor != "" {
		t.Errorf("SPEC.md 10.2: next_cursor is omitted when there are no more runs; got %q", got.NextCursor)
	}
}

// TestListRunFieldsMatchTheDatabase checks that every field API.md publishes
// carries the column it claims to.
//
// SPEC.md 17.1 makes database truth the interface, so the database is what the
// response is compared against - not a literal written into this test.
func TestListRunFieldsMatchTheDatabase(t *testing.T) {
	got := list(t, "?limit=200")
	for _, r := range got.Runs {
		row, err := harness.PSQL(
			"SELECT workflow_id || '|' || status || '|' || planner_attempt_count"+
				" || '|' || replay_count || '|' || coalesce(owner_id, '-')"+
				" FROM runs WHERE run_id = :'run';", "run="+r.RunID)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		owner := "-"
		if r.OwnerID != nil {
			owner = *r.OwnerID
		}
		want := fmt.Sprintf("%s|%s|%d|%d|%s",
			r.WorkflowID, r.Status, r.PlannerAttemptCount, r.ReplayCount, owner)
		if row != want {
			t.Errorf("run %s: the response disagrees with the database\n  db:   %s\n  http: %s",
				r.RunID, row, want)
		}
	}
}

// TestTerminalRunsHoldNoOwner is SPEC.md 6.2's invariant, asserted through the
// wire rather than in SQL: "owner_id and claimed_at are non-NULL only while
// status = 'RUNNING', and are always written and cleared as a pair".
//
// Every run in this fixture is terminal, so both must be null on every row. A
// client rendering "who owns this run" depends on it.
func TestTerminalRunsHoldNoOwner(t *testing.T) {
	for _, r := range list(t, "?limit=200").Runs {
		if r.Status == "RUNNING" {
			continue
		}
		if r.OwnerID != nil || r.ClaimedAt != nil {
			t.Errorf("SPEC.md 6.2: run %s is %s, so owner_id and claimed_at must both be null; got %v and %v",
				r.RunID, r.Status, r.OwnerID, r.ClaimedAt)
		}
	}
}

// TestStatusFilter is SPEC.md 10.2's `status` parameter, including its
// repeatability.
func TestStatusFilter(t *testing.T) {
	done := list(t, "?status=DONE")
	if len(done.Runs) != 1 || done.Runs[0].RunID != runDone {
		t.Errorf("SPEC.md 10.2: ?status=DONE must return only the DONE run; got %v", ids(done.Runs))
	}

	dlq := list(t, "?status=DLQ")
	if len(dlq.Runs) != 3 {
		t.Errorf("SPEC.md 10.2: ?status=DLQ must return the three DLQ runs; got %v", ids(dlq.Runs))
	}
	for _, r := range dlq.Runs {
		if r.Status != "DLQ" {
			t.Errorf("SPEC.md 10.2: ?status=DLQ returned a run in state %q", r.Status)
		}
	}

	both := list(t, "?status=DONE&status=DLQ")
	if len(both.Runs) != 4 {
		t.Errorf("SPEC.md 10.2: status is repeatable, so DONE and DLQ together must return all four; got %v",
			ids(both.Runs))
	}

	none := list(t, "?status=CANCELLED")
	if len(none.Runs) != 0 {
		t.Errorf("SPEC.md 10.2: no run was cancelled, so the list must be empty; got %v", ids(none.Runs))
	}
}

// TestUnknownStatusIsRejected is SPEC.md 10.2's own sentence — "an unknown
// status value is a 400, as §16 requires of every other input" — and SPEC.md
// 16's strictness principle behind it.
//
// Silently ignoring it would be worse than refusing: a client that typed DDLQ
// would be shown every run and believe it had filtered.
func TestUnknownStatusIsRejected(t *testing.T) {
	code, body, err := harness.Get("/runs?status=NONSENSE")
	if err != nil {
		t.Fatalf("GET /runs?status=NONSENSE: %v", err)
	}
	if code != http.StatusBadRequest {
		t.Fatalf("SPEC.md 10.2: an unknown status is a 400; got %d: %s", code, body)
	}
	var e errBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("SPEC.md 10.5: a rejection is JSON; got %s", body)
	}
	if e.Error != "invalid_request" {
		t.Errorf("SPEC.md 10.5: the slug is stable and machine-readable; expected invalid_request, got %q", e.Error)
	}
	if e.Message == "" {
		t.Errorf("SPEC.md 10.5: a rejection carries a human-readable message")
	}
}

// TestLimitBounds is SPEC.md 10.2's range: 1-200, default 50.
func TestLimitBounds(t *testing.T) {
	one := list(t, "?limit=1")
	if len(one.Runs) != 1 {
		t.Errorf("SPEC.md 10.2: limit=1 returns one run; got %d", len(one.Runs))
	}
	if one.NextCursor == "" {
		t.Errorf("SPEC.md 10.2: three runs remain, so next_cursor must be present")
	}

	for _, bad := range []string{"0", "201", "-1", "abc"} {
		code, body, err := harness.Get("/runs?limit=" + bad)
		if err != nil {
			t.Fatalf("GET /runs?limit=%s: %v", bad, err)
		}
		if code != http.StatusBadRequest {
			t.Errorf("SPEC.md 10.2: limit is 1-200, so %q is a 400; got %d: %s", bad, code, body)
		}
	}
}

// TestCursorWalksEveryRunExactlyOnce is what the ordering rule exists to make
// possible: "a cursor resumes exactly after the run it names" (SPEC.md 10.2).
//
// Paging one run at a time must visit all four, in the same order the unpaged
// list gives, with no run seen twice and none skipped. A page boundary that
// loses or repeats a row is the classic pagination defect, and it only shows up
// when the page size is smaller than the set.
func TestCursorWalksEveryRunExactlyOnce(t *testing.T) {
	want := ids(list(t, "?limit=200").Runs)

	var walked []string
	seen := map[string]bool{}
	query := "?limit=1"
	for i := 0; i < len(want)+2; i++ {
		page := list(t, query)
		for _, r := range page.Runs {
			if seen[r.RunID] {
				t.Fatalf("SPEC.md 10.2: run %s was returned on two pages\n  walked: %v", r.RunID, walked)
			}
			seen[r.RunID] = true
			walked = append(walked, r.RunID)
		}
		if page.NextCursor == "" {
			break
		}
		query = "?limit=1&cursor=" + page.NextCursor
	}

	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Errorf("SPEC.md 10.2: paging must visit every run once, in the same order\n  unpaged: %v\n  paged:   %v",
			want, walked)
	}
}

// ---------------------------------------------------------------------------
// GET /runs/{run_id}/dlq
// ---------------------------------------------------------------------------

func dlqOf(t *testing.T, runID string) dlqList {
	t.Helper()
	var out dlqList
	if err := harness.GetJSON("/runs/"+runID+"/dlq", &out); err != nil {
		t.Fatalf("GET /runs/%s/dlq: %v", runID, err)
	}
	if out.RunID != runID {
		t.Errorf("the response names run %s but %s was asked for", out.RunID, runID)
	}
	return out
}

// TestCleanRunHasAnEmptyHistory is the sentence SPEC.md 10.2 gained for this
// endpoint: a run with no entries is an empty list with a 200, not a 404,
// because SPEC.md 10.5 reserves 404 for an entity that does not exist and a run
// with a clean history exists.
func TestCleanRunHasAnEmptyHistory(t *testing.T) {
	code, body, err := harness.Get("/runs/" + runDone + "/dlq")
	if err != nil {
		t.Fatalf("GET dlq: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("SPEC.md 10.2: a run with no entries is a 200; got %d: %s", code, body)
	}
	var out dlqList
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unparseable: %v: %s", err, body)
	}
	if len(out.Entries) != 0 {
		t.Errorf("SPEC.md 6.5: the DONE run exhausted no budget, so it has no entries; got %d", len(out.Entries))
	}
	if out.Entries == nil && !strings.Contains(string(body), `"entries"`) {
		t.Errorf("the entries field must be present and empty, not absent: %s", body)
	}
}

// TestWorkerSideEntry is SPEC.md 6.5 and 12.3's worker side: the step used
// step_max_attempts without succeeding, so the entry names the step.
func TestWorkerSideEntry(t *testing.T) {
	got := dlqOf(t, runWorkerDLQ)
	if len(got.Entries) != 1 {
		t.Fatalf("SPEC.md 12.2: one exhausted budget writes one entry; got %d", len(got.Entries))
	}
	e := got.Entries[0]
	if e.Reason != "worker_budget_exhausted" {
		t.Errorf("SPEC.md 6.5: a step that exhausted its budget is worker_budget_exhausted; got %q", e.Reason)
	}
	if e.StepID == nil {
		t.Errorf("SPEC.md 6.5: a worker-side entry names the step that exhausted its budget; step_id was null")
	} else {
		row, err := harness.PSQL(
			"SELECT step_id FROM steps WHERE run_id = :'run' AND seq = 1;", "run="+runWorkerDLQ)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if row != *e.StepID {
			t.Errorf("SPEC.md 6.5: the entry must name this run's step; db %s, http %s", row, *e.StepID)
		}
	}
	if e.ReplayRound != 0 {
		t.Errorf("SPEC.md 6.5: nothing replayed this run, so the entry belongs to round 0; got %d", e.ReplayRound)
	}
	if e.ErrorText == "" {
		t.Errorf("SPEC.md 6.5: error_text is not null — the entry explains itself")
	}
	if len(e.ErrorText) > 4096 {
		t.Errorf("SPEC.md 6.4: error_text is truncated to 4 KB; got %d bytes", len(e.ErrorText))
	}
	if e.DLQID == "" || e.CreatedAt == "" {
		t.Errorf("SPEC.md 6.5: dlq_id and created_at are not null; got %q and %q", e.DLQID, e.CreatedAt)
	}
}

// TestPlannerSideEntryHasNoStep is the distinction the endpoint exists for.
//
// Both runs ended DLQ and runs.status cannot tell them apart. SPEC.md 6.5 says
// step_id is "NULL for a planner-side entry", and SPEC.md 12.3 is what makes
// the difference matter: one points at a worker, the other at a planner.
func TestPlannerSideEntryHasNoStep(t *testing.T) {
	got := dlqOf(t, runPlanrDLQ)
	if len(got.Entries) != 1 {
		t.Fatalf("SPEC.md 12.2: one exhausted planner budget writes one entry; got %d", len(got.Entries))
	}
	e := got.Entries[0]
	if e.Reason != "planner_budget_exhausted" {
		t.Errorf("SPEC.md 6.5: a paused planner exhausts planner_max_attempts, which is "+
			"planner_budget_exhausted; got %q", e.Reason)
	}
	if e.StepID != nil {
		t.Errorf("SPEC.md 6.5: step_id is NULL for a planner-side entry; got %q", *e.StepID)
	}
	if e.ErrorText == "" {
		t.Errorf("SPEC.md 6.5: error_text is not null")
	}

	// The two runs are indistinguishable by status, which is the whole reason
	// this endpoint carries the reason.
	worker := dlqOf(t, runWorkerDLQ)
	if len(worker.Entries) > 0 && worker.Entries[0].Reason == e.Reason {
		t.Errorf("SPEC.md 12.3: a worker-side and a planner-side death must be distinguishable; "+
			"both were %q", e.Reason)
	}
}

// TestHistoryAccumulatesAcrossReplayOldestFirst is the reason SPEC.md 10.2
// gives for this being its own endpoint: "it accumulates across replay rounds".
//
// The run is replayed while its worker is still broken, so it dies again and a
// second entry joins the first. SPEC.md 14 keeps the failed round's rows, and
// the amended SPEC.md 10.2 orders the result oldest first.
func TestHistoryAccumulatesAcrossReplayOldestFirst(t *testing.T) {
	before := dlqOf(t, runReplayDLQ)
	if len(before.Entries) != 1 {
		t.Fatalf("the fixture run should hold one entry before the replay; got %d", len(before.Entries))
	}

	count, err := harness.Replay(runReplayDLQ)
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if count != 1 {
		t.Errorf("SPEC.md 14: the first replay makes replay_count 1; got %d", count)
	}
	if _, err := harness.WaitTerminal(runReplayDLQ, harness.RunTimeout); err != nil {
		t.Fatalf("the replayed run did not reach a terminal state: %v", err)
	}

	after := dlqOf(t, runReplayDLQ)
	if len(after.Entries) != 2 {
		t.Fatalf("SPEC.md 10.2: the history accumulates across replay rounds; expected 2 entries, got %d",
			len(after.Entries))
	}
	if after.Entries[0].ReplayRound != 0 || after.Entries[1].ReplayRound != 1 {
		t.Errorf("SPEC.md 10.2: entries are oldest first, so round 0 precedes round 1; got %d then %d",
			after.Entries[0].ReplayRound, after.Entries[1].ReplayRound)
	}
	if after.Entries[0].DLQID != before.Entries[0].DLQID {
		t.Errorf("SPEC.md 14: a replay erases nothing, so the original entry must still be first")
	}
}

// TestUnknownRunIs404 is SPEC.md 10.5: 404 is "no such entity".
func TestUnknownRunIs404(t *testing.T) {
	code, body, err := harness.Get("/runs/00000000-0000-0000-0000-000000000000/dlq")
	if err != nil {
		t.Fatalf("GET dlq: %v", err)
	}
	if code != http.StatusNotFound {
		t.Fatalf("SPEC.md 10.5: a run that does not exist is a 404; got %d: %s", code, body)
	}
	var e errBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("SPEC.md 10.5: a rejection is JSON; got %s", body)
	}
	if e.Error != "not_found" {
		t.Errorf("SPEC.md 10.5: the slug is not_found; got %q", e.Error)
	}
}

// TestDLQMatchesTheDatabase compares the endpoint against the table it serves.
// SPEC.md 17.1 makes database truth the interface; this endpoint exists to
// carry it to a client that cannot open psql.
func TestDLQMatchesTheDatabase(t *testing.T) {
	for _, runID := range createdOrder {
		got := dlqOf(t, runID)
		row, err := harness.PSQL(
			"SELECT count(*) FROM dead_letter_queue WHERE run_id = :'run';", "run="+runID)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if fmt.Sprintf("%d", len(got.Entries)) != row {
			t.Errorf("run %s: the database holds %s entries, the endpoint returned %d",
				runID, row, len(got.Entries))
		}
	}
}

// ---------------------------------------------------------------------------
// The field the attempt summary was missing
// ---------------------------------------------------------------------------

// TestAttemptsCarryReplayRound covers a field that existed in the database and
// not on the wire.
//
// SPEC.md 6.4 gives every attempt a replay_round — "the value of
// runs.replay_count when this attempt was dispatched" — and SPEC.md 14 resets
// steps.attempt_count on replay, so after a replay the live counter no longer
// says what the failed round burned. Without this field a client cannot tell a
// round-0 attempt from a round-1 one, which is exactly what a replay demo has
// to show.
func TestAttemptsCarryReplayRound(t *testing.T) {
	var out struct {
		Steps []struct {
			StepID   string `json:"step_id"`
			Attempts []struct {
				AttemptNo   int `json:"attempt_no"`
				ReplayRound int `json:"replay_round"`
			} `json:"attempts"`
		} `json:"steps"`
	}
	if err := harness.GetJSON("/runs/"+runReplayDLQ+"/steps", &out); err != nil {
		t.Fatalf("GET steps: %v", err)
	}

	rounds := map[int]bool{}
	for _, s := range out.Steps {
		for _, a := range s.Attempts {
			rounds[a.ReplayRound] = true
			row, err := harness.PSQL(
				"SELECT replay_round FROM attempts WHERE step_id = :'step' AND attempt_no = "+
					fmt.Sprintf("%d", a.AttemptNo)+";", "step="+s.StepID)
			if err != nil {
				t.Fatalf("query failed: %v", err)
			}
			if row != fmt.Sprintf("%d", a.ReplayRound) {
				t.Errorf("SPEC.md 6.4: attempt %d of step %s is round %s in the database and %d on the wire",
					a.AttemptNo, s.StepID, row, a.ReplayRound)
			}
		}
	}
	if !rounds[0] || !rounds[1] {
		t.Errorf("SPEC.md 6.4, 14: this run was replayed, so its attempts should span rounds 0 and 1; saw %v",
			rounds)
	}
}
