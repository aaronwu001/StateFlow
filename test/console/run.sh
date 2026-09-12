#!/usr/bin/env bash
#
# The console suite - GET /runs and GET /runs/{run_id}/dlq.
#
#   ./test/console/run.sh                run every group
#   ./test/console/run.sh -run Cursor    pass any flag straight through
#
# Everything executes inside WSL (CLAUDE.md 8). The suite needs docker and the
# Go toolchain; it needs no psql, because it reaches the database through
# `docker compose exec postgres psql`.
#
# WHY -p 1
#   CLAUDE.md 5.5.1 brings up one docker-compose environment per group and tears
#   it down, volume wipe included, before the next starts. A Go package is one
#   group, and Go runs packages concurrently by default, so -p 1 is what makes
#   that discipline real rather than intended.
#
# WHY -count=1
#   A cached PASS would report that the contract still holds without having
#   started a container at all.

set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

for tool in docker go; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "run.sh: required tool not on PATH: $tool" >&2; exit 2; }
done

echo "The console suite - the read API"
echo "  environment: demos/console/docker-compose.yml (CLAUDE.md 5.5.4 - the same"
echo "  file the owner's hand-run demo uses; the suite defines none of its own)"
echo

exec go test -p 1 -count=1 -timeout 40m -v ./test/console/... "$@"
