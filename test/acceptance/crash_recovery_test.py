#!/usr/bin/env python3
"""
Owner-authored acceptance oracle — CRASH RECOVERY. Self-contained (uses the fake
planner + fake worker in this directory; no demo dependency).

STATUS: DRAFT ORACLE. The owner reviews the ASSERTIONS for intent now, and
smoke-tests this file once against the live stack after Session 6. From then it
is FROZEN: Claude Code must make it pass without editing it. If it fails, the
orchestrator is wrong.

Proves (assertions hit the frozen §14.1 DB schema, not the GET /runs JSON):
  1. A step finished before the crash is NOT re-run: its output + created_at are
     byte-identical across the crash.
  2. The mid-flight step gets an `orphaned` attempt, then reaches DONE
     (orphan-claim -> re-dispatch -> success).
  3. The run reaches DONE.
  4. No completed step has two DONE attempts (no duplicate checkpoint).
  5. No attempt is status=FAILED with a NULL reason (model invariant).

Requires: orchestrator + Postgres up; psql + docker on PATH. See _harness.py
for the host<->container networking notes (ADVERTISE_HOST).

ENV: see _harness.py, plus
  ORCH_CONTAINER   default stateflow
  CATCH_TIMEOUT_S  default 30
  FINISH_TIMEOUT_S default 120
"""
import os, subprocess, time
import _harness as H

ORCH = os.environ.get("ORCH_CONTAINER", "stateflow")
CATCH_TIMEOUT_S = int(os.environ.get("CATCH_TIMEOUT_S", "30"))
FINISH_TIMEOUT_S = int(os.environ.get("FINISH_TIMEOUT_S", "120"))


def main():
    # 3-step plan: sync ok -> async ok (delayed) -> sync ok. The async step is the
    # one we crash during. Worker delay 6s makes it catchable.
    plan = [
        {"name": "s1", "worker_url": H.worker_url("/sync/ok"), "mode": "sync",
         "timeout_seconds": 30, "input": {"k": "s1"}},
        {"name": "s2", "worker_url": H.worker_url("/async/ok"), "mode": "async",
         "timeout_seconds": 30, "input": {"k": "s2"}},
        {"name": "s3", "worker_url": H.worker_url("/sync/ok"), "mode": "sync",
         "timeout_seconds": 30, "input": {"k": "s3"}},
    ]
    with H.Fakes(plan, worker_delay_s=6) as fakes:
        wid = H.create_workflow(fakes.planner_endpoint())
        run_id = H.start_run(wid, {"text": "crash test"})
        print(f"[setup] run_id={run_id}")

        # catch mid-flight: >=1 DONE step AND a later step with a RUNNING attempt
        pre, deadline = None, time.time() + CATCH_TIMEOUT_S
        while time.time() < deadline:
            done = H.psql(f"SELECT step_name, output, created_at FROM steps "
                          f"WHERE run_id='{run_id}' AND status='DONE' ORDER BY seq")
            running = H.psql(
                f"SELECT s.step_name FROM steps s JOIN attempts a "
                f"ON a.attempt_id=s.current_attempt_id "
                f"WHERE s.run_id='{run_id}' AND s.status='RUNNING' AND a.status='RUNNING'")
            if done and running:
                d0 = done[0].split("\t")
                pre = {"name": d0[0], "output": d0[1], "created_at": d0[2],
                       "running": running[0].split("\t")[0]}
                break
            time.sleep(0.5)
        if not pre:
            H.die("could not catch DONE-step + RUNNING-async-step; raise worker delay")
        print(f"[catch] done={pre['name']} running={pre['running']} -> kill {ORCH}")

        subprocess.run(["docker", "kill", ORCH], check=True, capture_output=True)
        time.sleep(2)
        subprocess.run(["docker", "start", ORCH], check=True, capture_output=True)
        print("[restart] waiting for terminal state")

        final, deadline = None, time.time() + FINISH_TIMEOUT_S
        while time.time() < deadline:
            final = H.scalar(f"SELECT status FROM runs WHERE run_id='{run_id}'")
            if final in ("DONE", "DLQ"):
                break
            time.sleep(1.0)
        print(f"[final] run status={final}")

    # ---------------- FROZEN ASSERTIONS ----------------
    dn = pre["name"]
    post = H.psql(f"SELECT output, created_at FROM steps "
                  f"WHERE run_id='{run_id}' AND step_name='{dn}'")
    if not post:
        H.die(f"pre-crash DONE step {dn} vanished after restart")
    out, created = post[0].split("\t")
    if out != pre["output"]:
        H.die(f"step {dn} output changed across crash (re-run): {pre['output']!r} -> {out!r}")
    if created != pre["created_at"]:
        H.die(f"step {dn} created_at changed across crash (re-created)")
    H.ok(f"completed step {dn} untouched across crash (no re-run)")

    dup = H.scalar(f"SELECT count(*) FROM attempts a JOIN steps s ON a.step_id=s.step_id "
                   f"WHERE s.run_id='{run_id}' AND s.step_name='{dn}' AND a.status='DONE'")
    if dup != "1":
        H.die(f"completed step {dn} has {dup} DONE attempts (expected 1)")
    H.ok(f"completed step {dn} has exactly one DONE attempt")

    rn = pre["running"]
    orph = H.scalar(f"SELECT count(*) FROM attempts a JOIN steps s ON a.step_id=s.step_id "
                    f"WHERE s.run_id='{run_id}' AND s.step_name='{rn}' "
                    f"AND a.status='FAILED' AND a.failure_reason='orphaned'")
    if int(orph) < 1:
        H.die(f"mid-flight step {rn} has no orphaned attempt (recovery didn't claim it)")
    H.ok(f"mid-flight step {rn} has an orphaned attempt")

    rst = H.scalar(f"SELECT status FROM steps WHERE run_id='{run_id}' AND step_name='{rn}'")
    if rst != "DONE":
        H.die(f"mid-flight step {rn} did not reach DONE (status={rst})")
    H.ok(f"mid-flight step {rn} reached DONE after re-dispatch")

    if final != "DONE":
        H.die(f"run did not reach DONE (status={final})")
    H.ok("run reached DONE")

    bad = H.scalar(f"SELECT count(*) FROM attempts a JOIN steps s ON a.step_id=s.step_id "
                   f"WHERE s.run_id='{run_id}' AND a.status='FAILED' AND a.failure_reason IS NULL")
    if bad != "0":
        H.die(f"{bad} FAILED attempts with NULL reason (model violation)")
    H.ok("every FAILED attempt carries a reason")

    print("\nPASS: crash_recovery_test")


if __name__ == "__main__":
    main()
