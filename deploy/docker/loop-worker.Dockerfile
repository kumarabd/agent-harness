# Go loop-worker (Session Coordinator + Turn Workflow) — see loop-worker/.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/loop-worker.Dockerfile -t gcr.io/kumarabd/agent-harness/loop-worker:latest .

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src

# Copy module files first so `go mod download` is cached independently of
# source changes. go.work isn't copied here — loop-worker/go.mod's own
# `replace agent-harness/shared => ../shared` resolves the one cross-module
# dependency this image needs without requiring the whole workspace.
#
# Because of that, loop-worker/go.sum has to be self-sufficient — and a
# workspace build will NOT tell you when it isn't. go.work.sum supplies the
# entries this file is missing, so `go build ./...` passes locally while the
# image dies with "missing go.sum entry needed to verify package ... is
# provided by exactly one module". That is exactly how the genproto break
# reached a real export. After touching go.mod/go.sum, verify with the build
# this file actually runs, not the workspace one:
#   (cd loop-worker && GOWORK=off CGO_ENABLED=0 GOOS=linux go build ./cmd/loop-worker)
COPY shared/go.mod shared/go.sum ./shared/
COPY loop-worker/go.mod loop-worker/go.sum ./loop-worker/
RUN cd loop-worker && go mod download

COPY shared/ ./shared/
COPY loop-worker/ ./loop-worker/
RUN cd loop-worker && CGO_ENABLED=0 GOOS=linux go build -o /out/loop-worker ./cmd/loop-worker

# Distroless: no shell, no package manager — minimal attack surface for a
# process that will eventually hold no tenant credentials itself (this is the
# shared loop-worker, not the tenant-worker that holds tool credentials per
# docs/components/multi-tenancy.md) but should still not be trivially
# shell-accessible if compromised.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/loop-worker /loop-worker
USER nonroot:nonroot
ENTRYPOINT ["/loop-worker"]
