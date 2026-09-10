#!/usr/bin/env bash
#
# Milestone zeta - the automated suite.
#
# CLAUDE.md 4 step 6: this suite's job is to guarantee that what the owner saw
# by hand stays true. It does not replace step 5, and a green run the owner has
# never looked behind is not evidence the milestone landed.
#
#   ./test/zeta/run.sh              run every group
#   ./test/zeta/run.sh -run Gate    pass any flag straight through to go test
#
# Everything executes inside WSL (CLAUDE.md 8). The suite needs docker and the
# Go toolchain; it needs no psql, because it reaches the database through
# `docker compose exec postgres psql`, exactly as demo.sh does.
#
# WHY -p 1
#   Zeta is one group today, so -p 1 changes nothing about how it runs. It is
#   here because CLAUDE.md 5.5.1 makes the environment per-group and 5.5.2
#   requires each group to start from a clean database: a second group added to
#   this directory later must not come up beside this one and find port 8080
#   already published. The flag makes the group discipline structural rather
#   than a property of there happening to be only one.
#
# WHY -count=1
#   A cached PASS would report that a milestone still holds without having
#   started a container at all.
#
# WHY THE TIMEOUT IS LARGE
#   Zeta's environment adds a fourth service, the HTTP planner fixture, and
#   every step of an http-typed run makes a synchronous call to it (SPEC.md
#   9.2, 9.3) on top of whatever else a leg waits for. The first run of the day
#   also builds the orchestrator and planner fixture images from source.

set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

for tool in docker go; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "run.sh: required tool not on PATH: $tool" >&2; exit 2; }
done

echo "Milestone zeta - automated suite"
echo "  groups run one at a time, each against its own clean environment"
echo "  environment: demos/zeta/docker-compose.yml (CLAUDE.md 5.5.4 - the same"
echo "  file the owner's hand-run demo uses; the suite defines none of its own)"
echo

exec go test -p 1 -count=1 -timeout 40m -v ./test/zeta/... "$@"
