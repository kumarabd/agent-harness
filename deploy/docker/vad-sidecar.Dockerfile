# Silero VAD sidecar — see vad-sidecar/ and
# docs/components/gateway/discord-voice.md's "Resolved: Silero VAD". Runs as
# a second container in the same Gateway pod (deploy/helm/agent-harness-
# tenant/templates/gateway-deployment.yaml), not a shared cluster-wide
# service like whisper-svc/kokoro-svc — justified by VAD's ~50x/sec-per-
# speaker calling frequency, unlike those services' once-per-utterance
# pattern.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/vad-sidecar.Dockerfile -t gcr.io/kumarabd/agent-harness/vad-sidecar:latest .

FROM docker.io/library/python:3.12-slim AS build
WORKDIR /src

COPY vad-sidecar/pyproject.toml ./
RUN pip install --no-cache-dir --prefix=/install .

COPY vad-sidecar/vad_sidecar ./vad_sidecar
# vad_sidecar/pb/ (generated gRPC stubs, proto/vad_sidecar.proto) is
# committed, not regenerated here — this Dockerfile has no protoc/
# grpc_tools toolchain, and doesn't need one.

FROM docker.io/library/python:3.12-slim
# Not distroless, same reasoning as tenant-worker.Dockerfile: a real
# pip-installed dependency tree (onnxruntime, numpy), awkward to reproduce
# on a distroless base.
COPY --from=build /install /usr/local
COPY --from=build /src/vad_sidecar /app/vad_sidecar
WORKDIR /app

RUN useradd --system --no-create-home --uid 10002 vadsidecar
USER vadsidecar

ENTRYPOINT ["python", "-m", "vad_sidecar"]
