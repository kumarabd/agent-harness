# Go gateway (Web platform inbound/outbound path) — see workflows/cmd/gateway
# and docs/components/gateway/web.md.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/gateway.Dockerfile -t gcr.io/kumarabd/agent-harness/gateway:latest .

FROM golang:1.26-alpine AS build
WORKDIR /src

# Copy module files first so `go mod download` is cached independently of
# source changes.
COPY workflows/go.mod workflows/go.sum ./
RUN go mod download

COPY workflows/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/gateway ./cmd/gateway

# Distroless: no shell, no package manager. Unlike loop-worker, this process
# DOES hold this one tenant's Clerk secret key and talks to this tenant's own
# Postgres directly (docs/components/multi-tenancy.md's credential-isolation
# principle — one gateway per tenant, never shared) — minimal attack surface
# matters here at least as much.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
ENTRYPOINT ["/gateway"]
