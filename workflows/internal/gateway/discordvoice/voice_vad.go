package discordvoice

// voiceActivityDetector — docs/components/gateway/discord-voice.md's
// "Resolved: Audio Pipeline Shape": utterance boundaries are decided by a
// VAD scoring each captured frame, not a fixed silence timer alone. Behind
// an interface specifically so the interim implementation below can be
// swapped for a real neural VAD (Silero, per the design doc) later without
// touching any of the capture/utterance-boundary logic that calls it.
type voiceActivityDetector interface {
	// isSpeech reports whether a single voiceFrameSize-sample PCM frame
	// (interleaved, matching decodeVoiceFrame's output shape) contains
	// speech.
	isSpeech(frame []int16) bool

	// Err reports a fatal, sticky failure (docs/components/gateway/
	// discord-voice.md's "Resolved: Silero VAD" — fail loud, no silent
	// fallback). energyVAD never fails, so it always returns nil;
	// sileroVAD (voice_vad_silero.go) sets this once its sidecar becomes
	// unreachable, and the capture loop tears the connection down on it.
	Err() error
}

// energyVAD is a real, working interim implementation — a plain RMS-energy
// threshold, not the neural (Silero) VAD the design doc calls for. Chosen
// deliberately as the first implementation: it needs no model weights or
// ONNX runtime to stand up, so it has no external dependency this pass
// can't actually verify end-to-end. The interface above is what keeps
// swapping to Silero later a contained, isolated change rather than a
// rewrite of the surrounding capture loop.
type energyVAD struct {
	// threshold is a real starting value, not a tuned one — same
	// numeric-tuning deferral as every other undecided interval in this
	// project, pending real usage data.
	threshold float64
}

func newEnergyVAD() *energyVAD {
	return &energyVAD{threshold: 500}
}

// isSpeech compares mean-square energy against threshold^2 rather than
// computing an actual RMS (a sqrt) — same ordering, cheaper per frame, and
// this runs once per 20ms frame for every active speaker.
func (v *energyVAD) isSpeech(frame []int16) bool {
	if len(frame) == 0 {
		return false
	}
	var sumSquares float64
	for _, sample := range frame {
		s := float64(sample)
		sumSquares += s * s
	}
	meanSquare := sumSquares / float64(len(frame))
	return meanSquare > v.threshold*v.threshold
}

// Err always returns nil — a plain RMS threshold has no failure mode to report.
func (v *energyVAD) Err() error { return nil }
