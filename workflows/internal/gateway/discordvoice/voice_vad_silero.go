package discordvoice

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	vadpb "agent-harness/workflows/internal/vadpb"
)

// docs/components/gateway/discord-voice.md's "Resolved: Silero VAD" — a
// small Python sidecar (vad-sidecar/) colocated in the Gateway pod, called
// over gRPC per proto/vad_sidecar.proto. Empty means Silero isn't
// configured for this deployment at all — distinct from "configured but
// its sidecar is unreachable right now", which is a fail-loud runtime
// condition, not this.
func vadSidecarURL() string {
	return os.Getenv("VAD_SIDECAR_URL")
}

const (
	sileroFrameSamples   = 512 // fixed window size at 16kHz — the model's, not a tunable
	sileroContextSamples = 64
	sileroStateFloats    = 2 * 1 * 128
	// discord voice is 48kHz — see voice_opus.go's voiceSampleRate.
	sileroResampleRatio = voiceSampleRate / 16000 // 3
)

// sileroVADClient is shared across every speaker on a connection (and could
// be shared process-wide) — it holds nothing but a gRPC connection, no
// per-speaker state at all. Per-speaker state lives in sileroVAD itself
// (below), per the sidecar's own stateless-over-the-wire contract.
type sileroVADClient struct {
	conn        *grpc.ClientConn
	vad         vadpb.VADClient
	health      healthpb.HealthClient
	callTimeout time.Duration
}

// newSileroVADClient dials lazily-connecting (grpc.NewClient doesn't block
// or error on an unreachable target — failures surface per-call, which is
// exactly the fail-loud-per-call shape this design already wants) at
// target (e.g. "localhost:8500", no scheme — a plain gRPC dial target, not
// a URL despite the env var's name carried over from the HTTP prototype).
func newSileroVADClient(target string) *sileroVADClient {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		// grpc.NewClient only fails on a malformed target, not a dial
		// failure — treat it the same as any other unreachable-sidecar
		// case rather than panicking: every call against a nil conn (via
		// a no-op stub) below will simply return this same error path.
		return &sileroVADClient{callTimeout: 200 * time.Millisecond}
	}
	return &sileroVADClient{
		conn:        conn,
		vad:         vadpb.NewVADClient(conn),
		health:      healthpb.NewHealthClient(conn),
		callTimeout: 200 * time.Millisecond,
	}
}

// healthy is used once, at /join time (voiceJoin) — a real, current check
// via the standard grpc.health.v1 protocol, not a cached assumption, so a
// /join right after the sidecar container crashed is refused rather than
// silently proceeding.
func (c *sileroVADClient) healthy(ctx context.Context) bool {
	if c.health == nil {
		return false
	}
	resp, err := c.health.Check(ctx, &healthpb.HealthCheckRequest{Service: "vadsidecar.VAD"})
	if err != nil {
		return false
	}
	return resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
}

func f32ToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

func bytesToF32(b []byte, n int) ([]float32, error) {
	if len(b) != n*4 {
		return nil, fmt.Errorf("expected %d bytes, got %d", n*4, len(b))
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

func (c *sileroVADClient) classify(ctx context.Context, frame, state, context_ []float32) (probability float64, newState, newContext []float32, err error) {
	if c.vad == nil {
		return 0, nil, nil, fmt.Errorf("vad sidecar client not connected")
	}
	resp, err := c.vad.Classify(ctx, &vadpb.ClassifyRequest{
		Frame:   f32ToBytes(frame),
		State:   f32ToBytes(state),
		Context: f32ToBytes(context_),
	})
	if err != nil {
		return 0, nil, nil, err
	}
	newState, err = bytesToF32(resp.GetState(), sileroStateFloats)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("malformed state in sidecar response: %w", err)
	}
	newContext, err = bytesToF32(resp.GetContext(), sileroContextSamples)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("malformed context in sidecar response: %w", err)
	}
	return float64(resp.GetProbability()), newState, newContext, nil
}

// sileroVAD is one instance per speaker (docs' own design: Silero's ONNX
// model is a small RNN — its state must not be shared across independent
// speakers — unlike energyVAD, which is stateless and was fine shared
// across a whole connection). Implements voiceActivityDetector.
type sileroVAD struct {
	client  *sileroVADClient
	state   []float32
	context []float32

	// resampleBuf accumulates downmixed, resampled 16kHz mono samples
	// until there's enough for one 512-sample Silero window — Discord's
	// own 20ms frame (960 samples/channel @ 48kHz -> 320 mono samples @
	// 16kHz after resampling) doesn't divide evenly into 512, so window
	// boundaries don't line up with Discord's own frame boundaries.
	resampleBuf []float32
	lastVerdict bool

	// err is sticky once set — docs' "fail loud, not silent fallback":
	// once the sidecar has failed once, this speaker's detection is
	// considered broken, not silently retried into a corrupted state
	// stream (a partial/failed classify call must not corrupt the
	// state/context that feeds the next call).
	err error

	threshold float64
}

func newSileroVAD(client *sileroVADClient) *sileroVAD {
	return &sileroVAD{
		client:      client,
		state:       make([]float32, sileroStateFloats),
		context:     make([]float32, sileroContextSamples),
		resampleBuf: make([]float32, 0, sileroFrameSamples*2),
		threshold:   0.5,
	}
}

func (v *sileroVAD) Err() error { return v.err }

// isSpeech decodes frame — resample down to 16kHz mono. voiceFrameSize
// (voice_opus.go) is samples-per-channel; frame is interleaved stereo, so
// len(frame) == voiceFrameSize*voiceChannels.
func (v *sileroVAD) isSpeech(frame []int16) bool {
	if v.err != nil {
		return v.lastVerdict
	}
	mono := downmixResample(frame)
	v.resampleBuf = append(v.resampleBuf, mono...)

	for len(v.resampleBuf) >= sileroFrameSamples {
		window := v.resampleBuf[:sileroFrameSamples]
		v.resampleBuf = append([]float32{}, v.resampleBuf[sileroFrameSamples:]...)

		ctx, cancel := context.WithTimeout(context.Background(), v.client.callTimeout)
		prob, newState, newContext, err := v.client.classify(ctx, window, v.state, v.context)
		cancel()
		if err != nil {
			v.err = err
			return v.lastVerdict
		}
		v.state = newState
		v.context = newContext
		v.lastVerdict = prob > v.threshold
	}
	return v.lastVerdict
}

// downmixResample: stereo int16 (interleaved) -> mono float32 in [-1,1] ->
// crude box-filter decimation from 48kHz to 16kHz (average each group of
// sileroResampleRatio samples into one, a real anti-aliasing step, not a
// naive drop-2-of-3 decimation — exact fidelity doesn't matter for VAD, only
// that gross aliasing doesn't distort the speech-energy pattern Silero
// scores).
func downmixResample(frame []int16) []float32 {
	monoLen := len(frame) / voiceChannels
	mono := make([]float32, monoLen)
	for i := 0; i < monoLen; i++ {
		l := frame[i*voiceChannels]
		r := frame[i*voiceChannels+1]
		mono[i] = (float32(l) + float32(r)) / 2 / 32768.0
	}

	outLen := monoLen / sileroResampleRatio
	out := make([]float32, outLen)
	for i := 0; i < outLen; i++ {
		var sum float32
		for j := 0; j < sileroResampleRatio; j++ {
			sum += mono[i*sileroResampleRatio+j]
		}
		out[i] = sum / float32(sileroResampleRatio)
	}
	return out
}
