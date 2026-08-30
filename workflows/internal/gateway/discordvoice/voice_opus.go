package discordvoice

import "layeh.com/gopus"

// Discord's voice protocol is fixed, not a choice — docs/components/
// gateway/discord-voice.md's "Resolved: Sample Rate / Format Conversion":
// 48kHz, stereo, 20ms frames. voiceSampleRate * voiceFrameDurationMs / 1000
// gives voiceFrameSize (960) — the number of samples PER CHANNEL per frame,
// what discordgo's OpusSend/OpusRecv and gopus's Encode/Decode both expect.
const (
	voiceSampleRate  = 48000
	voiceChannels    = 2
	voiceFrameSizeMS = 20
	voiceFrameSize   = voiceSampleRate * voiceFrameSizeMS / 1000 // 960

	// voiceMaxOpusBytes is a generous upper bound for one encoded frame —
	// real Opus frames at this rate/size are far smaller in practice; this
	// is just gopus.Encoder's output buffer ceiling, not a tuned value.
	voiceMaxOpusBytes = 4000
)

// newVoiceOpusDecoder/newVoiceOpusEncoder are thin, intentionally narrow
// wrappers — every caller in this package always wants Discord's exact
// rate/channel/frame-size contract, so there's no reason to thread those
// three values through every call site individually.
func newVoiceOpusDecoder() (*gopus.Decoder, error) {
	return gopus.NewDecoder(voiceSampleRate, voiceChannels)
}

func newVoiceOpusEncoder() (*gopus.Encoder, error) {
	// gopus.Audio, not gopus.Voip — this is TTS-synthesized speech being
	// played back, not a live mic capture optimized for bandwidth-limited
	// voice; Audio mode is the better fit for already-clean input.
	return gopus.NewEncoder(voiceSampleRate, voiceChannels, gopus.Audio)
}

// decodeVoiceFrame decodes one Opus-encoded RTP packet payload into
// Discord's fixed-shape PCM frame (960 samples/channel, interleaved
// stereo). fec (forward error correction) is always false here — this
// package doesn't yet track per-SSRC sequence numbers to detect the dropped
// packet FEC exists to recover from; a dropped packet is just a dropped
// frame today, not a design decision, an acknowledged gap.
func decodeVoiceFrame(dec *gopus.Decoder, opusData []byte) ([]int16, error) {
	return dec.Decode(opusData, voiceFrameSize, false)
}

// encodeVoiceFrame encodes exactly one Discord-shaped PCM frame (must be
// voiceFrameSize*voiceChannels int16 samples — the caller is responsible for
// chunking a longer PCM stream into these before calling this per frame).
func encodeVoiceFrame(enc *gopus.Encoder, pcm []int16) ([]byte, error) {
	return enc.Encode(pcm, voiceFrameSize, voiceMaxOpusBytes)
}
