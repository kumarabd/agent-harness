package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// docs/components/gateway/discord-voice.md's "Resolved: True bidirectional
// realtime STT" — WhisperLive (infra/model/whisperlive/), not speaches'
// own /v1/realtime: that turned out to be a full OpenAI-Realtime-API-
// compatible speech-to-speech agent session (its own persona, turn
// detection, response generation) that couldn't be tamed into
// transcription-only mode, verified directly against the real running
// service. WhisperLive is purpose-built for the narrow thing actually
// needed: raw audio in over a plain WebSocket, partial + final transcript
// segments out. Empty means bidirectional STT isn't configured for this
// deployment — every utterance falls back to the existing batch
// transcribeAudio path (voice_stt_tts.go), same as before this existed.
func whisperLiveURL() string {
	return os.Getenv("WHISPERLIVE_URL")
}

// whisperLiveModel matches infra/model's own deployed model — "a good
// quality Whisper model," not the smaller/faster choice a purely latency-
// optimized deployment might make.
const whisperLiveModel = "large-v3"

// wlSegment is WhisperLive's real, verified wire shape (workflows/cmd/
// gateway/voice_stt_realtime.go's own testing against the live service,
// not the project's README) — a two-utterance test confirmed `completed`
// genuinely marks a real utterance boundary (transitions false->true right
// as a real silence gap was detected), not just a cosmetic flag.
type wlSegment struct {
	Start     string `json:"start"`
	End       string `json:"end"`
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

type wlServerMessage struct {
	UID      string      `json:"uid"`
	Message  string      `json:"message"`
	Backend  string      `json:"backend"`
	Status   string      `json:"status"`
	Segments []wlSegment `json:"segments"`
}

// whisperLiveSession is one per speaker, opened lazily on that speaker's
// first detected frame — same lifecycle timing as speakerBuffer/sileroVAD
// (discord_voice.go), decided directly after reconsidering the original
// "open proactively on channel join" design: that was justified
// specifically by WebRTC's heavy connection-setup cost, which stopped
// applying once this turned out to need only a plain, cheap WebSocket
// instead. One session per speaker because WhisperLive's own recognizer
// state is real, per-connection state that must not mix between
// independent speakers, the same reasoning as sileroVAD's own per-speaker
// instances.
type whisperLiveSession struct {
	url string

	mu       sync.Mutex
	conn     *websocket.Conn
	ready    bool
	segments []wlSegment
	// lastConsumedEnd — real, live bug fixed 2026-08-27: this used to be a
	// plain index (consumedCount) into segments[], which only stays valid
	// if segments[] only ever grows. It doesn't: the handshake's own
	// send_last_n_segments (10) makes the server send a SLIDING WINDOW of
	// the last 10 segments, and readLoop's own `s.segments = msg.Segments`
	// is a full replace, not an append — once a session produces more than
	// 10 real segments, len(segments) plateaus at 10 forever, and an index
	// already sitting at 10 from a prior call permanently satisfies
	// `consumedCount >= len(segments)` from then on. consumeNewText()
	// silently returned "" for the rest of that session's lifetime — no
	// error anywhere, just every later utterance quietly falling back to
	// batch STT, forever, confirmed live: gateway logs during a real
	// "goes dumb after about a minute" report showed zero WhisperLive
	// reconnects and zero errors of any kind, exactly what this bug
	// predicts (nothing ever resets consumedCount within one long-lived
	// session). Tracking progress by a segment's own End timestamp instead
	// survives the sliding window intact — a segment's End doesn't move
	// once the server has emitted it, regardless of where it sits in
	// whatever window gets echoed back. -1 means nothing consumed yet
	// (0.0 is a real, valid End for a genuine first segment).
	lastConsumedEnd float64
	err             error
}

func newWhisperLiveSession(url string) *whisperLiveSession {
	s := &whisperLiveSession{url: url}
	s.connect()
	return s
}

func (s *whisperLiveSession) connect() {
	conn, _, err := websocket.DefaultDialer.Dial(s.url, nil)
	if err != nil {
		s.mu.Lock()
		s.err = fmt.Errorf("dial: %w", err)
		s.mu.Unlock()
		return
	}

	// Real handshake shape, verified directly against the live service —
	// word_timestamps included even though it doesn't affect the server's
	// own now-disabled --batch_inference bug (infra/model/helm/templates/
	// whisperlive.yaml's own comment): it's still part of the real client
	// protocol, not something to omit just because this specific bug
	// didn't care about it.
	handshake := map[string]any{
		"uid":                   uuid.NewString(),
		"language":              "en",
		"task":                  "transcribe",
		"model":                 whisperLiveModel,
		"use_vad":               true,
		"send_last_n_segments":  10,
		"no_speech_thresh":      0.45,
		"clip_audio":            false,
		"same_output_threshold": 10,
		"word_timestamps":       false,
	}
	body, err := json.Marshal(handshake)
	if err != nil {
		conn.Close()
		s.mu.Lock()
		s.err = fmt.Errorf("marshal handshake: %w", err)
		s.mu.Unlock()
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, body); err != nil {
		conn.Close()
		s.mu.Lock()
		s.err = fmt.Errorf("send handshake: %w", err)
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	s.conn = conn
	// A reconnect (server DISCONNECT, or a fresh session) starts this
	// speaker's transcript over — real, accepted tradeoff: max_connection_
	// time is an hour (infra/model/helm/values.yaml), so this only ever
	// loses whatever text arrived in the narrow window between the last
	// consumeNewText() call and a same-instant disconnect, not anything
	// meaningful in normal operation.
	s.segments = nil
	s.lastConsumedEnd = -1
	s.err = nil
	s.ready = false
	s.mu.Unlock()

	go s.readLoop(conn)
}

func (s *whisperLiveSession) readLoop(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			if s.conn == conn { // a stale reader from a since-replaced connection — ignore
				s.err = fmt.Errorf("read: %w", err)
			}
			s.mu.Unlock()
			return
		}
		var msg wlServerMessage
		if jsonErr := json.Unmarshal(data, &msg); jsonErr != nil {
			continue
		}
		switch {
		case msg.Message == "SERVER_READY":
			s.mu.Lock()
			s.ready = true
			s.mu.Unlock()
		case msg.Message == "DISCONNECT":
			// Server-initiated close (its own max_connection_time budget) —
			// transparently reopen with a fresh uid rather than surfacing
			// this as a failure. The whole point of this session outliving
			// a single utterance is exactly this: a server-side connection
			// lifetime limit isn't a real error.
			log.Printf("discord-voice: whisperlive session disconnected by server, reconnecting")
			s.connect()
			return
		case len(msg.Segments) > 0:
			s.mu.Lock()
			s.segments = msg.Segments
			s.mu.Unlock()
		}
	}
}

// sendAudio streams one frame's worth of already-downmixed-and-resampled
// 16kHz mono float32 PCM — the exact same downmixResample output
// voice_vad_silero.go already produces for Silero, reused here rather
// than duplicating the conversion. A silent no-op before SERVER_READY or
// after a failure — the caller doesn't need to track connection state
// itself.
func (s *whisperLiveSession) sendAudio(mono16k []float32) {
	s.mu.Lock()
	conn := s.conn
	ready := s.ready
	failed := s.err != nil
	s.mu.Unlock()
	if conn == nil || !ready || failed {
		return
	}
	buf := make([]byte, len(mono16k)*4)
	for i, f := range mono16k {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
		s.mu.Lock()
		s.err = fmt.Errorf("send audio: %w", err)
		s.mu.Unlock()
	}
}

// consumeNewText returns the text of every segment not yet claimed by a
// prior call, joined into one string — Gateway's own VAD/silence-timeout
// (discord_voice.go, unchanged) still decides an utterance is over; this
// is what lets that decision read its answer from here instead of issuing
// a fresh batch transcription request. Completed and still-in-progress
// segments are joined the same way — by the time the silence timeout
// fires, the trailing in-progress segment is normally already stable.
//
// Walks the CURRENT sliding window by each segment's own End timestamp,
// not by a remembered array index — lastConsumedEnd's own comment has the
// real bug this replaces. A segment whose End fails to parse is skipped
// entirely (not emitted, doesn't advance lastConsumedEnd) rather than
// emitted unconditionally — WhisperLive's own real, verified wire shape
// always sends a numeric End, so this is only a hypothetical guard, but
// emitting an unparseable segment on every single call for as long as it
// sits in the window (nothing would ever mark it "seen") would trade one
// permanent-silence bug for a permanent-duplicate one.
//
// Real, un-tuned risk unchanged from before this fix: send_last_n_segments
// (10, the handshake above) bounds how much history the server retains —
// if consumeNewText is ever called this far behind a genuinely fast
// stream of segments, the oldest unconsumed ones could already have aged
// out of the window before being read at all. Not expected in practice
// (the caller consumes right as each utterance ends), and a real
// improvement over the old bug either way: aging out is a rare, bounded
// loss of stale text, not a permanent full stop for the rest of the
// session.
func (s *whisperLiveSession) consumeNewText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	newLastEnd := s.lastConsumedEnd
	for _, seg := range s.segments {
		end, err := strconv.ParseFloat(seg.End, 64)
		if err != nil || end <= s.lastConsumedEnd {
			continue
		}
		if t := strings.TrimSpace(seg.Text); t != "" {
			parts = append(parts, t)
		}
		if end > newLastEnd {
			newLastEnd = end
		}
	}
	s.lastConsumedEnd = newLastEnd
	return strings.TrimSpace(strings.Join(parts, " "))
}

// Err reports a sticky connection failure — checked by the caller purely
// for logging (docs' own design: STT falls back to the batch path on any
// failure here, not a fail-loud teardown like Silero VAD's — losing
// bidirectional STT's latency win for one utterance is a much smaller
// cost than losing barge-in/endpointing accuracy, which is what justified
// Silero's stricter treatment).
func (s *whisperLiveSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
