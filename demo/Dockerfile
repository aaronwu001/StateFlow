# Shared image for all demo Python processes (workers + LLM planner adapter).
# The specific script to run is supplied per-service via `command:` in
# docker-compose.demo.yml; behavior differences (name/port/delay/etc.) are
# supplied via environment variables, matching worker.py's existing
# env-only configuration contract.
FROM python:3.12-slim

# Force stdout/stderr unbuffered: without this, Python block-buffers stdout
# when it isn't a TTY (i.e. always, under Docker), so plain print() calls
# (llm_adapter.py's request logging in particular) can sit invisible in the
# buffer for a long time instead of showing up in `docker compose logs`.
ENV PYTHONUNBUFFERED=1

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY workers/ ./workers/
COPY planner/ ./planner/

ENTRYPOINT ["python"]
