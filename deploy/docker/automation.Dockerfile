# Go automation worker (self-serve tenant onboarding, TenantOnboardingWorkflow) —
# see workflows/cmd/automation and docs/components/gateway/web.md's Phase 2.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/automation.Dockerfile -t gcr.io/kumarabd/agent-harness/tenant-automation:latest .
#
# Unlike loop-worker/router (distroless/static — they hold no tenant
# credentials and never call the Kubernetes API), this image needs real
# `helm`/`kubectl` binaries and a shell: workflows/internal/automation/
# activities/exec.go deliberately shells out to them rather than adding a
# k8s.io/client-go or helm.sh/helm/v3 Go SDK dependency — see that file's
# own doc comment. Debian-slim, not Alpine, matching gateway.Dockerfile's
# own precedent for "this image genuinely needs more than a static Go
# binary."

FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY workflows/go.mod workflows/go.sum ./
RUN go mod download
COPY workflows/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/automation ./cmd/automation

FROM docker.io/library/debian:bookworm-slim

ARG KUBECTL_VERSION=v1.31.0
ARG HELM_VERSION=v3.16.2
ARG TARGETARCH=amd64

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl \
    && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
    && chmod +x /usr/local/bin/kubectl \
    && curl -fsSL "https://get.helm.sh/helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz" \
        | tar -xz -C /tmp \
    && mv "/tmp/linux-${TARGETARCH}/helm" /usr/local/bin/helm \
    && chmod +x /usr/local/bin/helm \
    && rm -rf /tmp/linux-${TARGETARCH} \
    && apt-get purge -y curl && apt-get autoremove -y && rm -rf /var/lib/apt/lists/*

# The actual chart sources HelmInstallTenant/RegisterSharedPoolNamespace run
# `helm upgrade --install`/`helm upgrade` against (workflows/internal/
# automation/activities/helm.go, TENANT_CHART_DIR/SHARED_CHART_DIR env vars)
# — includes each chart's own vendored charts/*.tgz dependencies
# (postgresql, agent-brain, mcp-hub), so no network access to any OCI
# registry is needed at runtime, only to the cluster's own Temporal/
# Kubernetes API.
COPY deploy/helm/agent-harness-tenant /charts/agent-harness-tenant
COPY deploy/helm/agent-harness-shared /charts/agent-harness-shared

RUN useradd --system --no-create-home --uid 65532 automation
COPY --from=build /out/automation /automation
USER automation
ENTRYPOINT ["/automation"]
