package main

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	eotpb "agent-harness/workflows/internal/eotpb"
)

// docs/components/gateway/discord-voice.md's "In Progress: Turn-Taking
// Model" — an audio-native end-of-turn classifier (LiveKit's turn-detector
// v1-mini, self-hosted, no cloud call), served as a second gRPC service on
// the same vad-sidecar process/port that already serves Silero VAD
// (server.py's own doc comment). Reuses the exact same target string as
// vadSidecarURL() — one sidecar, two services, one dial target — but keeps
// its own separate *grpc.ClientConn rather than sharing sileroVADClient's,
// matching this codebase's existing one-client-type-per-service pattern.
//
// eotThreshold — the probability at or above which the EOT model's verdict
// alone is enough to end an utterance before voiceUtteranceSilenceFrames'
// ceiling. LiveKit's own calibrated English default is 0.36 (`languages.py`
// in `livekit-agents`). Verified directly against real audio (this doc's
// own Notes Log): a complete sentence scored 0.392, a deliberately
// trailed-off sentence 0.274, a lone backchannel "Yeah" 0.119, a lone
// filler "Um" 0.047.
//
// Kept at LiveKit's calibrated 0.36. A 2026-08-29 tuning pass briefly set
// this to 0.40 — a mistake: the model's own measured "complete sentence"
// score is 0.392, so 0.40 is ABOVE it, meaning EOT would never fire on a
// normal finished sentence and every turn would fall through to the full
// voiceUtteranceSilenceFrames ceiling (a latency regression on every turn).
// This threshold must stay at or below the measured "done" score to be
// useful at all. To buy more patience without breaking EOT, raise the grace
// period / ceiling (voiceUtteranceEOTGraceFrames / voiceUtteranceSilenceFrames),
// not this.
const eotThreshold = 0.36

// eotMaxSamples mirrors the model's own fixed rolling window
// (livekit.local_inference.EOT_MAX_SAMPLES, 19200 samples = 1.2s @ 16kHz) —
// re-declared here rather than fetched from the sidecar at runtime, since
// it's a fixed property of the exact model version this project pins to,
// not something that varies per call.
const eotMaxSamples = 19200

// eotTrailingRawSamples is how much of a speaker's raw 48kHz-stereo buffer
// (speakerBuffer.pcm) actually needs to be resampled before a predict()
// call — eotMaxSamples of 16kHz mono audio, converted back through
// downmixResample's own ratio (sileroResampleRatio, voice_vad_silero.go)
// and channel count. Real bug fixed 2026-08-27: the capture loop's own EOT
// check was resampling the ENTIRE accumulated utterance on every poll, not
// just the trailing window the model actually looks at — cost that grows
// without bound the longer a speaker keeps talking, run while holding the
// one mutex that gates every speaker's frame processing on the connection.
// A long utterance made every later EOT poll progressively more expensive,
// live evidence being a 30-second-long `processing` lifecycle hold that
// reset to `listening` with no transcript at all — consistent with audio
// getting delayed badly enough during that stretch that batch STT had
// nothing usable left to transcribe. predict() itself already
// zero-pads the front when handed fewer samples than eotMaxSamples, so
// slicing here changes nothing about correctness, only cost.
const eotTrailingRawSamples = eotMaxSamples * sileroResampleRatio * voiceChannels

// eotClient wraps the gRPC connection to the sidecar's EOT service. Holds
// no per-speaker state at all — unlike sileroVADClient's own per-call
// state/context threading, every Predict call here is a fresh, independent
// classification of whatever window the caller sends; this struct is safe
// to share across every speaker on a connection (or process-wide).
type eotClient struct {
	conn        *grpc.ClientConn
	eot         eotpb.EOTClient
	callTimeout time.Duration
}

// newEOTClient dials lazily, same reasoning as newSileroVADClient: a
// gRPC target string, not a URL — grpc.NewClient never blocks or errors on
// an unreachable target, so failures surface per-call, matching this
// feature's own fail-OPEN posture (see shouldEndTurn's own comment) rather
// than needing a connect-time check the way voiceJoin's Silero health
// check does for VAD's fail-loud posture.
func newEOTClient(target string) *eotClient {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return &eotClient{callTimeout: 300 * time.Millisecond}
	}
	return &eotClient{
		conn:        conn,
		eot:         eotpb.NewEOTClient(conn),
		callTimeout: 300 * time.Millisecond,
	}
}

// predict windows pcm (int16, 16kHz mono — the caller's own job to get it
// into that shape) down to exactly eotMaxSamples, zero-padding at the FRONT
// (oldest end) for a shorter utterance so the model always sees "real audio
// ends here, at the most recent sample" — verified against real audio this
// exact way (this file's own eotThreshold comment has the real numbers),
// not independently confirmed against LiveKit's own internal ring-buffer
// padding behavior, which isn't observable from outside their client.
func (c *eotClient) predict(ctx context.Context, pcm []int16) (probability float64, err error) {
	if c.eot == nil {
		return 0, errEOTNotConnected
	}
	window := pcm
	if len(window) > eotMaxSamples {
		window = window[len(window)-eotMaxSamples:]
	}
	buf := make([]byte, eotMaxSamples*2)
	offset := eotMaxSamples - len(window)
	for i, s := range window {
		idx := (offset + i) * 2
		buf[idx] = byte(uint16(s))
		buf[idx+1] = byte(uint16(s) >> 8)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	resp, err := c.eot.Predict(callCtx, &eotpb.PredictRequest{Pcm: buf})
	if err != nil {
		return 0, err
	}
	return float64(resp.GetProbability()), nil
}

var errEOTNotConnected = errStr("eot sidecar client not connected")

type errStr string

func (e errStr) Error() string { return string(e) }

// downmixResample16kMono converts one Discord-shaped stereo int16 frame
// buffer (any length, interleaved) to mono float32 at 16kHz using the exact
// same box-filter decimation voice_vad_silero.go's downmixResample already
// uses for Silero — reused by name there for a single 20ms frame; this
// helper is the multi-frame form needed to downsample a whole accumulated
// utterance buffer (speakerBuffer.pcm) in one pass, then converts back to
// int16 (the EOT model's own expected dtype, unlike Silero's float32) since
// the two models simply disagree on input representation.
func downmixResample16kMonoInt16(stereoInt16 []int16) []int16 {
	mono := downmixResample(stereoInt16) // voice_vad_silero.go — reused verbatim, same 48kHz->16kHz ratio
	out := make([]int16, len(mono))
	for i, f := range mono {
		v := f * 32768.0
		switch {
		case v > 32767:
			v = 32767
		case v < -32768:
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
}
