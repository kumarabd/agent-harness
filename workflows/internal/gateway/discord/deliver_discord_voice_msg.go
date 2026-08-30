package discord

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"mime/multipart"

	"github.com/bwmarrin/discordgo"
)

// sendDiscordVoiceMessage posts one Ogg/Opus clip to a channel as a NATIVE
// Discord voice message — the inline waveform-player bubble, not a plain
// audio file card — for docs/components/gateway/discord.md's "Resolved:
// Per-Channel Reply Mode".
//
// This builds the multipart request by hand rather than going through
// discordgo's ChannelMessageSendComplex: Discord requires an `attachments`
// array carrying `duration_secs` + `waveform` alongside the IS_VOICE_MESSAGE
// flag (1<<13 = 8192) or it rejects the message, and discordgo's MessageSend
// struct has no field to express that metadata on a send. session.RequestRaw
// still gives us discordgo's auth + rate-limit handling around the raw body.
func sendDiscordVoiceMessage(session *discordgo.Session, channelID string, ogg []byte) (*discordgo.Message, error) {
	contentType, body, err := buildVoiceMessageMultipart(ogg)
	if err != nil {
		return nil, err
	}
	endpoint := discordgo.EndpointChannelMessages(channelID)
	resp, err := session.RequestRaw("POST", endpoint, contentType, body, endpoint, 0)
	if err != nil {
		return nil, err
	}
	var msg discordgo.Message
	if err := json.Unmarshal(resp, &msg); err != nil {
		return nil, fmt.Errorf("sendDiscordVoiceMessage: decoding response: %w", err)
	}
	return &msg, nil
}

// buildVoiceMessageMultipart assembles the Discord voice-message request
// body: a `payload_json` field carrying the IS_VOICE_MESSAGE flag (8192) and
// the required per-attachment duration_secs + waveform, plus the Ogg/Opus
// file itself as `files[0]` (the index matching attachments[0].id).
func buildVoiceMessageMultipart(ogg []byte) (contentType string, body []byte, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	payload := fmt.Sprintf(
		`{"flags":8192,"attachments":[{"id":"0","filename":"voice-message.ogg","duration_secs":%.2f,"waveform":%q}]}`,
		oggOpusDurationSecs(ogg), placeholderWaveform(),
	)
	if err = w.WriteField("payload_json", payload); err != nil {
		return "", nil, err
	}
	part, err := w.CreateFormFile("files[0]", "voice-message.ogg")
	if err != nil {
		return "", nil, err
	}
	if _, err = part.Write(ogg); err != nil {
		return "", nil, err
	}
	if err = w.Close(); err != nil {
		return "", nil, err
	}
	return w.FormDataContentType(), buf.Bytes(), nil
}

// oggOpusDurationSecs reads the last Ogg page's granule position — for Opus
// this is a sample count at a fixed 48kHz regardless of the encoder's own
// rate — and converts it to seconds. Best-effort: returns 0 if the bytes
// don't parse (Discord still accepts the message, it just shows no
// duration). The ~80ms pre-skip in the OpusHead is not subtracted; it does
// not matter for Discord's own duration display.
func oggOpusDurationSecs(ogg []byte) float64 {
	last := bytes.LastIndex(ogg, []byte("OggS"))
	if last < 0 || last+14 > len(ogg) {
		return 0
	}
	granule := binary.LittleEndian.Uint64(ogg[last+6 : last+14])
	if granule == ^uint64(0) { // 0xFFFF... = "no packet completes on this page"
		return 0
	}
	return float64(granule) / 48000.0
}

// placeholderWaveform is the little bar graph Discord renders on the
// voice-message bubble. Computing a real amplitude envelope would mean
// demuxing the Ogg and decoding the Opus (no demuxer wired into this path);
// a gentle static pattern reads better than a dead-flat block, and Discord
// accepts any 1..256 bytes of base64 here.
func placeholderWaveform() string {
	b := make([]byte, 40)
	for i := range b {
		b[i] = byte(90 + (i*37)%60) // mid amplitude, shallow rolling pattern
	}
	return base64.StdEncoding.EncodeToString(b)
}
