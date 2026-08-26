# Go loop-worker (Session Coordinator + Turn Workflow) — see workflows/cmd/loop-worker.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/loop-worker.Dockerfile -t gcr.io/kumarabd/agent-harness/loop-worker:latest .

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src

# Copy module files first so `go mod download` is cached independently of
# source changes.
COPY workflows/go.mod workflows/go.sum ./
RUN go mod download

COPY workflows/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/loop-worker ./cmd/loop-worker

# Distroless: no shell, no package manager — minimal attack surface for a
# process that will eventually hold no tenant credentials itself (this is the
# shared loop-worker, not the tenant-worker that holds tool credentials per
# docs/components/multi-tenancy.md) but should still not be trivially
# shell-accessible if compromised.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/loop-worker /loop-worker
USER nonroot:nonroot
ENTRYPOINT ["/loop-worker"]
