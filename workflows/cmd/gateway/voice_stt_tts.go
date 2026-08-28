package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
)

// Speech-to-text and text-to-speech clients — docs/components/gateway/
// discord-voice.md's "Resolved: STT/TTS as a Standard, Provider-Agnostic
// Contract". Designed against the OpenAI-compatible Audio API shape
// (/v1/audio/transcriptions, /v1/audio/speech), the same de facto standard
// most STT/TTS providers implement — mirrors how activities/activities/
// llm.py and workers/mining_common/llm.py already treat chat completions: a
// thin client against a standard shape, provider swappable via config alone
// (base URL + model name), never a provider-specific integration.
// whisper-large-v3/kokoro are already registered on the shared litellm
// proxy specifically because that's litellm's whole purpose — normalizing
// diverse backends behind this exact contract.
//
// Verified for real against the live services (2026-08-25, port-forwarded
// whisper-svc/kokoro-svc directly — both are the `speaches` project):
// /v1/audio/transcriptions genuinely supports stream=true (real SSE,
// multiple `data: {"text": "..."}` events for a multi-sentence utterance —
// output streamed as segments are decoded, not the input; the full audio
// still has to be uploaded first); /v1/audio/speech's sample_rate parameter
// genuinely returns raw PCM at exactly the requested rate (confirmed 48kHz
// mono via duration math against a known sentence), and the response is
// natively chunked, so it can be consumed incrementally rather than
// buffered whole.
var (
	speechBaseURL = envOrDefault("SPEECH_BASE_URL", "http://litellm-service.core.svc:4000/v1")
	speechAPIKey  = os.Getenv("SPEECH_API_KEY")
	sttModel      = envOrDefault("STT_MODEL", "whisper-large-v3")
	ttsModel      = envOrDefault("TTS_MODEL", "kokoro")
	// ttsVoice deliberately has no default — Kokoro's own voice-name
	// convention isn't confirmed (OpenAI's fixed enum, e.g. "alloy", almost
	// certainly doesn't apply to a different provider), so guessing one
	// would be worse than omitting the field and letting the server apply
	// its own default when unset.
	ttsVoice = os.Getenv("TTS_VOICE")
)

// transcribeAudio uploads a WAV file's bytes to {SPEECH_BASE_URL}/audio/transcriptions
// with stream=true and returns the concatenated transcript text once every
// segment has arrived. This streams the server's OUTPUT, not its input —
// docs/components/gateway.md's "Resolved: Voice Platforms — Cascaded
// Architecture" is explicit that this isn't true bidirectional streaming
// (the whole utterance is still uploaded before any event arrives, so
// discord_voice.go's VAD-buffered capture is unchanged) — what this buys is
// lower time-to-first-result for a long, multi-sentence utterance instead
// of waiting for the entire transcription to finish before getting anything
// back. Callers that only need the final text (today, the only kind) see no
// interface change from the old single-JSON-response version; the segments
// aren't currently exposed individually since nothing downstream consumes
// them yet (docs/components/gateway.md's own "don't call the LLM per
// delta" endpointing principle — VAD decides turn completion, not STT
// segment boundaries).
func transcribeAudio(ctx context.Context, wavBytes []byte) (string, error) {
	return transcribeAudioFile(ctx, wavBytes, "audio.wav")
}

// transcribeAudioFile is transcribeAudio generalized to an explicit
// filename — added for discord_voice_message.go's own use, transcribing
// Discord's recorded-and-uploaded voice-message attachments (real Ogg/Opus
// files, not the raw-PCM WAV shape discord_voice.go's own VAD-buffered
// utterances always produce). whisper-svc (the `speaches` project) picks
// its decode path from the uploaded filename's own extension, not just the
// multipart part's content-type, so transcribeAudio's original hardcoded
// "audio.wav" would have silently mis-decoded any non-WAV input — this is
// the one thing that hardcoding couldn't express. Same OpenAI-compatible
// /v1/audio/transcriptions contract and streaming-segment handling either
// way.
func transcribeAudioFile(ctx context.Context, audioBytes []byte, filename string) (string, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audioBytes); err != nil {
		return "", err
	}
	if err := writer.WriteField("model", sttModel); err != nil {
		return "", err
	}
	if err := writer.WriteField("stream", "true"); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", speechBaseURL+"/audio/transcriptions", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if speechAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+speechAPIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("transcribeAudioFile: %s: %s", resp.Status, string(respBody))
	}

	// Real SSE, verified directly: "data: {\"text\": \"...\"}\n\n" per
	// segment, blank-line-separated. bufio.Scanner's default line-splitting
	// handles this fine — every real line either starts with "data: " or is
	// the blank separator between events, nothing else observed.
	var segments []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			// A malformed individual event shouldn't sink the whole
			// transcript — skip it, same tolerance discord.go's own
			// best-effort logging elsewhere in this package uses.
			continue
		}
		if event.Text != "" {
			segments = append(segments, event.Text)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("transcribeAudioFile: error reading stream: %w", err)
	}
	return strings.Join(segments, " "), nil
}

// synthesizeSpeechPCM POSTs text to {SPEECH_BASE_URL}/audio/speech
// requesting raw PCM. Still sends sample_rate=voiceSampleRate below, but it
// is NOT honored (confirmed live 2026-08-26 — identical byte output with,
// without, or through litellm's proxy vs. calling kokoro-svc directly) —
// left in the request as a harmless forward-compat hint in case a future
// kokoro-svc version starts respecting it, but nothing downstream may rely
// on that: the real synthesized audio is always Kokoro's native 24kHz
// (voice_convert.go's kokoroSampleRate), and deliver_voice.go upsamples it
// to 48kHz itself before Opus encoding. Returns the still-open response
// body for the caller to read incrementally (deliver_voice.go Opus-encodes
// and sends frame-by-frame as bytes arrive, rather than buffering the
// whole response first) — the response is natively chunked (verified
// directly), so this is a real pipelining win, not just an API nicety. The
// caller owns closing the returned body.
//
// Mono, not stereo — Kokoro (and TTS models generally) synthesize a single
// voice; voice_convert.go's monoToStereoPCM does the (trivial, no longer
// ffmpeg-dependent) upmix to Discord's required channel count, after
// upsample2xPCM handles the sample-rate correction above.
func synthesizeSpeechPCM(ctx context.Context, text string) (io.ReadCloser, error) {
	payload := map[string]any{
		"model":           ttsModel,
		"input":           text,
		"response_format": "pcm",
		"sample_rate":     voiceSampleRate,
	}
	if ttsVoice != "" {
		payload["voice"] = ttsVoice
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", speechBaseURL+"/audio/speech", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/octet-stream")
	if speechAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+speechAPIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("synthesizeSpeechPCM: %s: %s", resp.Status, string(respBody))
	}
	return resp.Body, nil
}
