# syntax=docker/dockerfile:1

# --- Build stage -------------------------------------------------------
FROM golang:1.25 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Pure Go (pgx stdlib driver, no CGO deps) — static binary for distroless.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/stateflow ./cmd/stateflow

# --- Runtime stage -------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/stateflow /stateflow

EXPOSE 8080

ENTRYPOINT ["/stateflow"]
