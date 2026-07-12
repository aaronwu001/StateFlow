package orchestrator

// White-box unit tests for boundHistory (registry #1, whitepaper §12.2): the
// two-tier size guard applied to RunState.History on every planner.Decide
// call. package orchestrator (not orchestrator_test) so these tests can
// exercise the unexported boundHistory function directly, the same way
// effectiveTimeout's logic is exercised only indirectly elsewhere — here we
// want precise, DB-free coverage of the pure size-guard function itself.
//
// No TEST_DATABASE_URL is required for anything in this file — boundHistory
// is a pure function over already-loaded core.HistoryEntry values.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aaronwu000/stateflow/internal/core"
)

// bytesOutput returns a json.RawMessage of a JSON string literal whose
// marshaled length is exactly n bytes: a JSON string is `"` + content + `"`,
// so content must be n-2 bytes of a safe (non-escaping) filler character.
func bytesOutput(n int) []byte {
	if n < 2 {
		panic("bytesOutput: n must be >= 2 to hold the surrounding quotes")
	}
	return []byte(`"` + strings.Repeat("x", n-2) + `"`)
}

// TestBoundHistory_UnderBothCaps_PassesThroughUnchanged is mandatory test
// (a): the common case (small history, small outputs) must be byte-for-byte
// untouched by boundHistory.
func TestBoundHistory_UnderBothCaps_PassesThroughUnchanged(t *testing.T) {
	in := []core.HistoryEntry{
		{Name: "alpha", Status: "DONE", Output: json.RawMessage(`{"a":1}`)},
		{Name: "beta", Status: "DONE", Output: json.RawMessage(`{"b":2}`)},
	}
	out := boundHistory(in)

	if len(out) != len(in) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Name != in[i].Name {
			t.Errorf("entry %d: Name = %q, want %q", i, out[i].Name, in[i].Name)
		}
		if out[i].Status != in[i].Status {
			t.Errorf("entry %d: Status = %q, want %q", i, out[i].Status, in[i].Status)
		}
		if string(out[i].Output) != string(in[i].Output) {
			t.Errorf("entry %d: Output = %s, want %s (unchanged)", i, out[i].Output, in[i].Output)
		}
	}
	t.Logf("PASS — %d entries under both caps passed through byte-for-byte unchanged", len(in))
}

// TestBoundHistory_OverPerEntryCap_GetsPointer is mandatory test (b): an
// entry whose Output exceeds historyEntryCapBytes is replaced with the
// pointer object carrying the correct size_bytes.
func TestBoundHistory_OverPerEntryCap_GetsPointer(t *testing.T) {
	bigSize := historyEntryCapBytes + 500
	in := []core.HistoryEntry{
		{Name: "big", Status: "DONE", Output: bytesOutput(bigSize)},
	}
	out := boundHistory(in)

	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	var ptr historyTruncationPointer
	if err := json.Unmarshal(out[0].Output, &ptr); err != nil {
		t.Fatalf("entry 0 Output did not unmarshal as a pointer object: %v (raw: %s)", err, out[0].Output)
	}
	if !ptr.Truncated {
		t.Errorf("ptr.Truncated = false, want true")
	}
	if ptr.SizeBytes != bigSize {
		t.Errorf("ptr.SizeBytes = %d, want %d (the ORIGINAL marshaled size)", ptr.SizeBytes, bigSize)
	}
	if !strings.Contains(ptr.Note, "GET /runs/{run_id}") {
		t.Errorf("ptr.Note = %q, want it to reference GET /runs/{run_id}", ptr.Note)
	}
	t.Logf("PASS — %d-byte output replaced with pointer: %s", bigSize, out[0].Output)
}

// TestBoundHistory_OverTotalCap_OldestEntriesDropOutputEntirely is mandatory
// test (c): a long run whose cumulative (post-per-entry-cap) size exceeds
// historyTotalCapBytes keeps Output (per-entry-capped as needed) on the most
// recent entries and drops Output ENTIRELY (no pointer, no field) on older
// ones, once the remaining budget can't fit them.
func TestBoundHistory_OverTotalCap_OldestEntriesDropOutputEntirely(t *testing.T) {
	// Each entry sits at 1000 bytes — under the 2048 per-entry cap, so tier 1
	// never fires; only tier 2 (total cap) is under test here. 51200 / 1000
	// = 51.2, so 52 such entries push cumulative size past the 50KB total
	// cap; the oldest ones must drop Output.
	const perEntry = 1000
	const n = 60
	in := make([]core.HistoryEntry, n)
	for i := 0; i < n; i++ {
		in[i] = core.HistoryEntry{Name: entryName(i), Status: "DONE", Output: bytesOutput(perEntry)}
	}

	out := boundHistory(in)
	if len(out) != n {
		t.Fatalf("len(out) = %d, want %d — total cap must never drop ENTRIES, only their Output", len(out), n)
	}

	// Walk most-recent-first (index n-1 down to 0) and confirm: a contiguous
	// run of newest entries keep Output (and it's unchanged, since perEntry
	// is under the per-entry cap), and everything older has no Output field
	// at all (nil, so omitempty drops it from the wire).
	var used int
	firstDroppedIdx := -1
	for i := n - 1; i >= 0; i-- {
		if out[i].Output == nil {
			if firstDroppedIdx == -1 {
				firstDroppedIdx = i
			}
			continue
		}
		if firstDroppedIdx != -1 {
			t.Fatalf("entry %d has Output after an older-index entry %d already had it dropped — "+
				"drop boundary must be contiguous from the oldest end", i, firstDroppedIdx)
		}
		if len(out[i].Output) != perEntry {
			t.Errorf("entry %d: Output len = %d, want %d (unchanged, under per-entry cap)", i, len(out[i].Output), perEntry)
		}
		used += len(out[i].Output)
	}

	if firstDroppedIdx == -1 {
		t.Fatalf("no entry had Output dropped — total cap (%d bytes) should have been exceeded by %d entries of %d bytes each",
			historyTotalCapBytes, n, perEntry)
	}
	if used > historyTotalCapBytes {
		t.Errorf("kept Output totals %d bytes, exceeds the %d-byte total cap", used, historyTotalCapBytes)
	}
	// Sanity: the kept prefix (from the most-recent end) should be close to
	// the budget — not trivially small. floor(51200/1000) = 51 entries.
	wantKept := historyTotalCapBytes / perEntry
	gotKept := n - 1 - firstDroppedIdx
	if gotKept != wantKept {
		t.Errorf("kept %d most-recent entries' Output, want %d (51200/%d)", gotKept, wantKept, perEntry)
	}
	t.Logf("PASS — %d entries of %d bytes each (%d total, over the %d-byte cap): "+
		"most recent %d kept Output (%d bytes used), older %d entries dropped Output entirely",
		n, perEntry, n*perEntry, historyTotalCapBytes, gotKept, used, n-gotKept)
}

// TestBoundHistory_SeqOrderPreservedRegardlessOfCuts is mandatory test (d):
// the returned slice's order (by construction, seq ascending — the order
// LoadFrontier already returns) is never reordered by boundHistory, whether
// or not any entry was cut by either tier.
func TestBoundHistory_SeqOrderPreservedRegardlessOfCuts(t *testing.T) {
	in := []core.HistoryEntry{
		{Name: "seq0", Status: "DONE", Output: bytesOutput(1000)},
		{Name: "seq1", Status: "DONE", Output: bytesOutput(historyEntryCapBytes + 100)}, // tier 1 cuts this one
		{Name: "seq2", Status: "DONE", Output: bytesOutput(1000)},
	}
	out := boundHistory(in)

	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	wantNames := []string{"seq0", "seq1", "seq2"}
	for i, want := range wantNames {
		if out[i].Name != want {
			t.Errorf("out[%d].Name = %q, want %q — order must stay seq ascending regardless of truncation", i, out[i].Name, want)
		}
	}
	t.Logf("PASS — order preserved (seq0, seq1, seq2) even though seq1 was per-entry-capped")
}

// TestBoundHistory_EmptyHistory_ReturnsEmpty is a basic boundary regression
// check: an empty history must not panic and must round-trip as empty.
func TestBoundHistory_EmptyHistory_ReturnsEmpty(t *testing.T) {
	out := boundHistory([]core.HistoryEntry{})
	if len(out) != 0 {
		t.Fatalf("len(out) = %d, want 0", len(out))
	}
}

// entryName builds a deterministic, sortable step name for the total-cap
// test's synthetic history.
func entryName(i int) string {
	return "step" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}
