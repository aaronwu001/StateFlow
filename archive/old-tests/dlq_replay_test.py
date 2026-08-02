#!/usr/bin/env python3
"""
Owner-authored acceptance oracle — DLQ EXHAUSTION + REPLAY RESET. Self-contained
(fake planner + fake worker; no demo dependency).

STATUS: DRAFT ORACLE (see crash_recovery_test.py header). Frozen after the owner
smoke-tests it against the live stack. Claude Code must make it pass unmodified.

Proves (assertions hit the frozen §14.1 DB schema):
  1. An always-failing worker drives the step to exactly X FAILED attempts, then
     step->DLQ + run->DLQ + one dead_letter_queue row reason='worker_retry_exhausted'
     with non-empty context (per-attempt reasons). (TX3, §7.1)
  2. POST /dlq/{id}/replay performs TX5: attempt_count RESET to 0, step+run back to
     RUNNING, a new attempt created. The "reset is mandatory, not decorative" (§11)
     property — asserted directly, deterministic against an always-fail worker.

ENV: see _harness.py, plus
  EXPECT_X               required — the retry limit X you configured (see below)
  DLQ_EXTRA_CONFIG_JSON  default '{"retry_limit": 2}'  (CONFIRM: how X is set in
                         planner_config — key name is impl-defined, confirm at S6)
  DLQ_TIMEOUT_S          default 90
"""
import json, os, time
import _harness as H

EXPECT_X = int(os.environ["EXPECT_X"])
EXTRA = json.loads(os.environ.get("DLQ_EXTRA_CONFIG_JSON", '{"retry_limit": 2}'))
DLQ_TIMEOUT_S = int(os.environ.get("DLQ_TIMEOUT_S", "90"))


def main():
    # single step pointing at the always-fail sync worker
    plan = [{"name": "boom", "worker_url": H.worker_url("/sync/fail"),
             "mode": "sync", "timeout_seconds": 10, "input": {"k": "boom"}}]
    with H.Fakes(plan, worker_delay_s=1) as fakes:
        wid = H.create_workflow(fakes.planner_endpoint(), extra_config=EXTRA)
        run_id = H.start_run(wid, {"text": "dlq test"})
        print(f"[setup] run_id={run_id} expecting X={EXPECT_X}")

        status, deadline = None, time.time() + DLQ_TIMEOUT_S
        while time.time() < deadline:
            status = H.scalar(f"SELECT status FROM runs WHERE run_id='{run_id}'")
            if status == "DLQ":
                break
            if status == "DONE":
                H.die("run reached DONE but worker always fails")
            time.sleep(1.0)
        if status != "DLQ":
            H.die(f"run did not reach DLQ within {DLQ_TIMEOUT_S}s (status={status})")
        H.ok("run reached DLQ")

        # ---- exhaustion (TX3) ----
        srow = H.psql(f"SELECT step_id, status, attempt_count FROM steps WHERE run_id='{run_id}'")
        if len(srow) != 1:
            H.die(f"expected one step, found {len(srow)}")
        step_id, sstat, cnt = srow[0].split("\t")
        if sstat != "DLQ":
            H.die(f"step status {sstat}, expected DLQ")
        if int(cnt) != EXPECT_X:
            H.die(f"attempt_count {cnt}, expected X={EXPECT_X}")
        H.ok(f"step in DLQ with attempt_count={EXPECT_X}")

        nf = H.scalar(f"SELECT count(*) FROM attempts WHERE step_id='{step_id}' AND status='FAILED'")
        if int(nf) != EXPECT_X:
            H.die(f"{nf} FAILED attempts, expected X={EXPECT_X}")
        H.ok(f"exactly {EXPECT_X} FAILED attempts")

        dlq = H.psql(f"SELECT id, reason, context FROM dead_letter_queue WHERE run_id='{run_id}'")
        if len(dlq) != 1:
            H.die(f"expected one DLQ row, found {len(dlq)}")
        dlq_id, reason, context = dlq[0].split("\t")
        if reason != "worker_retry_exhausted":
            H.die(f"DLQ reason {reason}, expected worker_retry_exhausted")
        if not context or context in ("{}", "null"):
            H.die("DLQ context empty; must carry per-attempt reasons")
        H.ok("one DLQ row, reason=worker_retry_exhausted, context populated")

        # ---- replay = TX5 reset ----
        H.http_post(f"/dlq/{dlq_id}/replay", {})
        seen, deadline = False, time.time() + 15
        while time.time() < deadline:
            row = H.scalar(f"SELECT status||'|'||attempt_count FROM steps WHERE step_id='{step_id}'")
            st, c = row.split("|")
            if st == "RUNNING" and c == "0":
                seen = True
                break
            time.sleep(0.2)
        if not seen:
            H.die("after replay never observed RUNNING + attempt_count=0 (TX5 reset missing)")
        H.ok("replay reset attempt_count to 0 and returned step to RUNNING (TX5)")

    print("\nPASS: dlq_replay_test")


if __name__ == "__main__":
    main()
