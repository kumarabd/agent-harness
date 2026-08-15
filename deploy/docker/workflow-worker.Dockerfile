# Go workflow worker (Session Coordinator + Turn Workflow) — see workflows/cmd/worker.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/workflow-worker.Dockerfile -t gcr.io/kumarabd/agent-harness/workflow-worker:latest .

FROM golang:1.26-alpine AS build
WORKDIR /src

# Copy module files first so `go mod download` is cached independently of
# source changes.
COPY workflows/go.mod workflows/go.sum ./
RUN go mod download

COPY workflows/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/worker ./cmd/worker

# Distroless: no shell, no package manager — minimal attack surface for a
# process that will eventually hold no tenant credentials itself (this is the
# workflow layer, not the activity layer that holds tool credentials per
# docs/components/multi-tenancy.md) but should still not be trivially
# shell-accessible if compromised.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/worker /worker
USER nonroot:nonroot
ENTRYPOINT ["/worker"]
