package discordvoice

import (
	"bytes"
	"encoding/binary"
)

// pcmToWAV wraps raw interleaved 16-bit PCM samples in a minimal WAV
// (RIFF/WAVE) container — the standard 44-byte header, no extra chunks.
// Used for the ingestion side (docs/components/gateway/discord-voice.md's
// "Resolved: Sample Rate / Format Conversion"): Discord's own decoded PCM
// is already 48kHz stereo, so it needs packaging into a real container for
// upload, not resampling — the standard /v1/audio/transcriptions contract's
// server side is expected to handle whatever sample rate the container
// declares.
func pcmToWAV(pcm []int16, sampleRate, channels int) []byte {
	dataSize := len(pcm) * 2 // 2 bytes per sample (16-bit)
	byteRate := sampleRate * channels * 2
	blockAlign := channels * 2

	buf := &bytes.Buffer{}
	buf.WriteString("RIFF")
	binary.Write(buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	binary.Write(buf, binary.LittleEndian, uint32(16)) // PCM fmt chunk size
	binary.Write(buf, binary.LittleEndian, uint16(1))  // PCM format tag
	binary.Write(buf, binary.LittleEndian, uint16(channels))
	binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(buf, binary.LittleEndian, uint32(byteRate))
	binary.Write(buf, binary.LittleEndian, uint16(blockAlign))
	binary.Write(buf, binary.LittleEndian, uint16(16)) // bits per sample

	buf.WriteString("data")
	binary.Write(buf, binary.LittleEndian, uint32(dataSize))
	binary.Write(buf, binary.LittleEndian, pcm)

	return buf.Bytes()
}

// monoToStereoPCM upmixes mono 16-bit PCM to interleaved stereo by
// duplicating each sample to both channels — replaces what used to be an
// ffmpeg subprocess call.
//
// CORRECTION (2026-08-26): the 2026-08-25 comment that used to be here
// claimed sample_rate=voiceSampleRate made this the only remaining
// conversion step, verified via duration math. That verification was
// wrong — re-tested live and confirmed kokoro-svc does NOT honor
// sample_rate at all, called directly or through litellm's proxy (byte-
// for-byte identical output regardless of the parameter, or its absence).
// Real, deployed voice output was garbled — playing back at roughly double
// speed, sounding like "fast noise" — because of exactly this: the actual
// PCM is Kokoro's native 24kHz, but was being encoded as if it were 48kHz.
// upsample2xPCM below now runs before this function; ffmpeg's removal
// itself is still correct (this doesn't need a full resampler, just a
// cheap 2x-specific one), but the "no conversion needed at all" framing
// was not.
func monoToStereoPCM(mono []int16) []int16 {
	stereo := make([]int16, len(mono)*2)
	for i, s := range mono {
		stereo[i*2] = s
		stereo[i*2+1] = s
	}
	return stereo
}

// kokoroSampleRate is kokoro-svc's real native output rate — confirmed
// live (2026-08-26) via duration math against a known sentence (byte count
// / 2 bytes-per-sample / 24000 matched a natural speaking pace; the same
// math against 48000 implied roughly double-speed, matching the garbled
// audio actually heard), consistent across both a direct call and one
// routed through litellm. Not derived from any documented spec — Kokoro's
// real behavior, verified against the real service, same discipline as
// every other "verified, not assumed" finding in this codebase. Re-verify
// if kokoro-svc's own image/model version ever changes.
const kokoroSampleRate = 24000

// upsample2xPCM doubles the sample rate via linear interpolation between
// consecutive samples — kokoroSampleRate (24kHz) to voiceSampleRate (48kHz)
// is exactly 2x, so a general-purpose resampler isn't needed, just this.
// Must run before monoToStereoPCM above on every frame of synthesized
// speech now that sample_rate is confirmed not to be honored server-side.
//
// Per-chunk, not a continuous stream: the last input sample in each call
// has no "next" sample to interpolate toward (this function carries no
// state across calls the way a real streaming resampler would), so it's
// just repeated — a small, accepted seam at each 20ms frame boundary, not
// worth the added statefulness to smooth out.
func upsample2xPCM(mono []int16) []int16 {
	if len(mono) == 0 {
		return nil
	}
	out := make([]int16, len(mono)*2)
	for i, s := range mono {
		out[i*2] = s
		if i+1 < len(mono) {
			out[i*2+1] = int16((int32(s) + int32(mono[i+1])) / 2)
		} else {
			out[i*2+1] = s
		}
	}
	return out
}

// pcmBytesToInt16 decodes a raw little-endian 16-bit PCM byte slice (as
// read directly off speech.SynthesizePCM's response body) into samples.
// Odd trailing byte, if any, is dropped rather than erroring — the last
// chunk read off a streaming HTTP response can legitimately end mid-sample
// if the caller's read buffer size doesn't divide the total evenly; the
// caller is responsible for carrying that leftover byte into the next read
// if exact sample alignment matters (deliver_voice.go's frame loop does).
func pcmBytesToInt16(b []byte) []int16 {
	n := len(b) / 2
	samples := make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(b[i*2 : i*2+2]))
	}
	return samples
}
