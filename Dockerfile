# syntax=docker/dockerfile:1

# --- Build stage -------------------------------------------------------
FROM golang:1.25 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY migrations/ ./migrations/

# Pure Go (pgx stdlib driver, no CGO deps) — static binary for distroless.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/stateflow ./cmd/stateflow

# --- Runtime stage -------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/stateflow /stateflow

EXPOSE 8080

# Distroless has no shell, curl, or wget, so the probe is the binary itself
# in its `healthcheck` subcommand (cmd/stateflow/main.go's runHealthcheck):
# a self HTTP GET against GET /healthz, exit 0/non-zero. Closes Temporary
# Design Registry item #8 (whitepaper §18).
HEALTHCHECK --interval=5s --timeout=3s --start-period=5s --retries=5 \
    CMD ["/stateflow", "healthcheck"]

ENTRYPOINT ["/stateflow"]
