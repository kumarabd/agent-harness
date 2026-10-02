# Go router (shared, identity-routing front door for agent-web) — see
# router/ and docs/components/gateway/web.md.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/router.Dockerfile -t gcr.io/kumarabd/agent-harness/router:latest .

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src

# Copy module files first so `go mod download` is cached independently of
# source changes. go.work isn't copied here — router/go.mod's own `replace`
# directives for agent-harness/shared and agent-harness/automation resolve
# both cross-module dependencies this image needs without requiring the
# whole workspace.
COPY shared/go.mod shared/go.sum ./shared/
COPY automation/go.mod automation/go.sum ./automation/
COPY router/go.mod router/go.sum ./router/
RUN cd router && go mod download

COPY shared/ ./shared/
COPY automation/ ./automation/
COPY router/ ./router/
RUN cd router && CGO_ENABLED=0 GOOS=linux go build -o /out/router .

# Distroless: this process holds no tenant credentials at all (only a
# namespace/release-name pointer per organization in its own tenant_registry
# table), same reasoning as loop-worker.Dockerfile.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/router /router
USER nonroot:nonroot
ENTRYPOINT ["/router"]
