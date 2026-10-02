# Python tenant-worker (ModelCall, ToolCall, InsertMessage, Persist,
# CompressContext) — see tenant-worker/tenant_worker/__main__.py.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/tenant-worker.Dockerfile -t gcr.io/kumarabd/agent-harness/tenant-worker:latest .

FROM docker.io/library/python:3.12-slim AS build
WORKDIR /src

# Copy project metadata first so dependency install is cached independently
# of source changes.
COPY tenant-worker/pyproject.toml ./
RUN pip install --no-cache-dir --prefix=/install .

COPY tenant-worker/tenant_worker ./tenant_worker

FROM docker.io/library/python:3.12-slim
# Not distroless: this layer pip-installs a real dependency tree
# (temporalio and its transitive deps), which is awkward to reproduce
# correctly on a distroless base — python:3.12-slim is the pragmatic standard
# choice here, unlike the Go binary above which has zero runtime dependencies.
COPY --from=build /install /usr/local
COPY --from=build /src/tenant_worker /app/tenant_worker
WORKDIR /app

RUN useradd --system --no-create-home --uid 10001 worker
USER worker

ENTRYPOINT ["python", "-m", "tenant_worker"]
