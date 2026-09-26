# Go router (shared, identity-routing front door for agent-web) — see
# workflows/cmd/router and docs/components/gateway/web.md.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/router.Dockerfile -t gcr.io/kumarabd/agent-harness/router:latest .

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src

# Copy module files first so `go mod download` is cached independently of
# source changes.
COPY workflows/go.mod workflows/go.sum ./
RUN go mod download

COPY workflows/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/router ./cmd/router

# Distroless: this process holds no tenant credentials at all (only a
# namespace/release-name pointer per organization in its own tenant_registry
# table), same reasoning as loop-worker.Dockerfile.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/router /router
USER nonroot:nonroot
ENTRYPOINT ["/router"]
