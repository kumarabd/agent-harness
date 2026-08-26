package main

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
// ffmpeg subprocess call. No longer needed as of 2026-08-25: verified
// directly against the real kokoro-svc that requesting response_format=pcm
// with sample_rate=voiceSampleRate returns raw PCM already at Discord's
// exact rate (confirmed via duration math against a known sentence) — the
// only real conversion work left is mono→stereo, which doesn't need a
// resampler at all, just this duplication. deploy/docker/gateway.Dockerfile's
// ffmpeg dependency was removed along with this function's old
// implementation.
func monoToStereoPCM(mono []int16) []int16 {
	stereo := make([]int16, len(mono)*2)
	for i, s := range mono {
		stereo[i*2] = s
		stereo[i*2+1] = s
	}
	return stereo
}

// pcmBytesToInt16 decodes a raw little-endian 16-bit PCM byte slice (as
// read directly off synthesizeSpeechPCM's response body) into samples.
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
