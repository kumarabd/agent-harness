package discord

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"

	"agent-harness/workflows/internal/gateway/speech"
)

// discordVoiceMessageFetchTimeout bounds the download+transcribe round trip
// for one voice-message attachment — a real starting value, not tuned
// against real usage yet, same category as every other undecided interval
// in this project (voiceUtteranceSilenceFrames, discordConnectionLeaseTTL,
// ...). Generous relative to a typical short voice note, but bounded so a
// slow/stalled CDN fetch or a slow STT call can't hang discordMessageCreate
// indefinitely — discordgo dispatches MessageCreate handlers synchronously
// off its own event loop, so a hang here would delay every later event on
// this connection, not just this one message.
// 2026-08-30: raised from 30s to 60s. The download is fast; the transcribe
// leg goes through litellm to whisper-svc on a GPU that is currently also
// serving live-voice STT and TTS (and, as of late Aug, an unrelated
// non-Kubernetes workload — see discord-voice.md's 2026-08-29 Notes Log), so
// a real Ogg/Opus voice note can queue well past 30s under contention.
const discordVoiceMessageFetchTimeout = 60 * time.Second

// discordVoiceMessageMaxBytes caps the downloaded attachment size before it
// reaches speech.TranscribeFile — mirrors activities/activities/tools.py's own
// flat truncation safety cap for tool output: a hard ceiling against a
// pathological upload, not a claim about typical size. 25MB matches
// Whisper's own commonly-documented upload limit, so a file this rejects is
// one the STT service would likely reject anyway.
const discordVoiceMessageMaxBytes = 25 * 1024 * 1024

// discordVoiceMessageContent resolves a Discord voice-message MessageCreate
// into real transcript text — gateway/discord.md's "Attachments/stickers/
// audio as real triggering content" plan, the voice-message third of that
// plan, built out. Reuses the exact STT contract gateway/discord-voice.md
// already established (speech.TranscribeFile → {SPEECH_BASE_URL}'s
// OpenAI-compatible /v1/audio/transcriptions) rather than inventing a
// second transcription path.
//
// Deliberately distinct from that doc's own live pipeline: this is a
// recorded-and-uploaded clip attached to an ordinary text-channel message
// (Discord's own voice-message feature — MessageFlagsIsVoiceMessage, a real
// Ogg/Opus file), downloaded and transcribed once as a whole file, not
// captured frame-by-frame off a live RTP voice connection the way
// discord_voice.go's capture loop works.
//
// Returns ("", nil) — not an error — when m isn't actually a voice message
// (wrong flag, or no attachment), so a caller can treat "no voice content
// here" and "a real, non-voice message" identically without a separate
// boolean.
func discordVoiceMessageContent(ctx context.Context, m *discordgo.MessageCreate) (string, error) {
	if m.Flags&discordgo.MessageFlagsIsVoiceMessage == 0 || len(m.Attachments) == 0 {
		return "", nil
	}
	att := m.Attachments[0]

	ctx, cancel := context.WithTimeout(ctx, discordVoiceMessageFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, att.URL, nil)
	if err != nil {
		return "", fmt.Errorf("discordVoiceMessageContent: building download request: %w", err)
	}
	// Discord's CDN can 403 a request with no User-Agent.
	req.Header.Set("User-Agent", "DiscordBot (agent-harness, 1.0)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("discordVoiceMessageContent: downloading attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discordVoiceMessageContent: attachment download %s from %q (content-type %q, %d bytes)",
			resp.Status, att.URL, att.ContentType, att.Size)
	}

	audioBytes, err := io.ReadAll(io.LimitReader(resp.Body, discordVoiceMessageMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("discordVoiceMessageContent: reading attachment body: %w", err)
	}
	if len(audioBytes) > discordVoiceMessageMaxBytes {
		return "", fmt.Errorf("discordVoiceMessageContent: attachment exceeds %d byte cap", discordVoiceMessageMaxBytes)
	}

	filename := att.Filename
	if filename == "" {
		// Discord's own voice-message attachments are always real Ogg/Opus
		// (content_type "audio/ogg; codecs=opus") even on the rare chance the
		// filename itself comes back empty — matches the extension
		// speech.TranscribeFile needs to pick the right decode path.
		filename = "voice-message.ogg"
	}

	text, err := speech.TranscribeFile(ctx, audioBytes, filename)
	if err != nil {
		return "", fmt.Errorf("discordVoiceMessageContent: transcribing: %w", err)
	}
	return text, nil
}
