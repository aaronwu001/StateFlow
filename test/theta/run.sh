#!/usr/bin/env bash
#
# Milestone theta - the automated suite.
#
# CLAUDE.md 4 step 6: this suite's job is to guarantee that what the owner saw
# by hand stays true. It does not replace step 5, and a green run the owner has
# never looked behind is not evidence the milestone landed.
#
#   ./test/theta/run.sh                 run every group
#   ./test/theta/run.sh -run RawBody    pass any flag straight through to go test
#
# Everything executes inside WSL (CLAUDE.md 8). The suite needs docker and the
# Go toolchain; it needs no psql, because it reaches the database through
# `docker compose exec postgres psql`, exactly as demo.sh does.
#
# WHY -p 1
#   CLAUDE.md 5.5.1 brings up one docker-compose environment per group and tears
#   it down, volume wipe included, before the next group starts; 5.5.2 says
#   groups never share a live environment. Go runs test binaries from different
#   packages concurrently by default, and each group here is a package, so the
#   default would have two groups bring up the same compose project at once.
#   -p 1 is what makes the group discipline real rather than intended.
#
# WHY -count=1
#   A cached PASS would report that a milestone still holds without having
#   started a container at all.

set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

for tool in docker go; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "run.sh: required tool not on PATH: $tool" >&2; exit 2; }
done

echo "Milestone theta - automated suite"
echo "  groups run one at a time, each against its own clean environment"
echo "  environment: demos/theta/docker-compose.yml (CLAUDE.md 5.5.4 - the same"
echo "  file the owner's hand-run demo uses; the suite defines none of its own)"
echo

exec go test -p 1 -count=1 -timeout 40m -v ./test/theta/... "$@"
