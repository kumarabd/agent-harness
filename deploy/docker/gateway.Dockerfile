# Go gateway (Web + Discord text/voice platform inbound/outbound path) —
# see workflows/cmd/gateway and docs/components/gateway/{web,discord,discord-voice}.md.
#
# Build context must be the REPO ROOT, not deploy/docker/:
#   docker build -f deploy/docker/gateway.Dockerfile -t gcr.io/kumarabd/agent-harness/gateway:latest .

FROM golang:1.26-bookworm AS build
WORKDIR /src

# libopus-dev: docs/components/gateway/discord-voice.md's "Resolved: Audio
# Pipeline Shape" — layeh.com/gopus is a real cgo binding to libopus, not a
# pure-Go decoder, chosen deliberately (the mature reference implementation
# over a reimplementation). This is what forces CGO_ENABLED=1 below and the
# switch away from Alpine (musl's libopus packaging is a worse fit than
# Debian's here).
RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config libopus-dev \
    && rm -rf /var/lib/apt/lists/*

# Copy module files first so `go mod download` is cached independently of
# source changes.
COPY workflows/go.mod workflows/go.sum ./
RUN go mod download

COPY workflows/ ./
RUN CGO_ENABLED=1 GOOS=linux go build -o /out/gateway ./cmd/gateway

# NOT distroless/static — the cgo libopus binding above needs libopus's
# shared library present at runtime, not just link time, and distroless/
# static has no shared libraries at all (not even glibc). This used to also
# need a real `ffmpeg` binary (an external program, categorically
# incompatible with distroless/static) — removed 2026-08-25 once
# voice_stt_tts.go started requesting PCM at Discord's exact sample rate
# directly from Kokoro instead of resampling locally (verified against the
# real service: sample_rate is honored exactly), leaving mono→stereo
# duplication as the only remaining conversion, which needs no external
# process at all (voice_convert.go's monoToStereoPCM, pure Go). libopus0
# alone is a real, much smaller attack-surface increase over the original
# distroless/static choice than the ffmpeg-era tradeoff was — still not
# zero, but debian-slim's `apt-get install` is what correctly resolves
# libopus0's own transitive dependencies across architectures, which a
# hand-copied .so from the build stage would risk getting subtly wrong.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    libopus0 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --no-create-home --uid 65532 gateway
COPY --from=build /out/gateway /gateway
USER gateway
ENTRYPOINT ["/gateway"]
