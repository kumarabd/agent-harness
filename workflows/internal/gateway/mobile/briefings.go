package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agent-harness/workflows/internal/gateway/clerkauth"
	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/gateway/speech"
)

const maxBriefings = 10

type briefing struct {
	TurnSeq     int       `json:"turn_seq"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	AudioPath   string    `json:"audio_path"`
	PublishedAt time.Time `json:"published_at"`
}

type briefingResponse struct {
	Briefings []briefing `json:"briefings"`
}

// authenticatedUser is deliberately local to this native-only adapter. The
// Web package's equivalent helper is private and its context key must not be
// shared across platform packages. Both verify the same Clerk token contract.
func (h *Handler) authenticatedUser(r *http.Request) (string, error) {
	authorization := r.Header.Get("Authorization")
	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == "" || token == authorization {
		return "", errors.New("missing bearer token")
	}
	return clerkauth.VerifyJWT(r.Context(), h.clerk, token)
}

func (h *Handler) handleBriefings(w http.ResponseWriter, r *http.Request) {
	userID, err := h.authenticatedUser(r)
	if err != nil {
		http.Error(w, "invalid session token", http.StatusUnauthorized)
		return
	}

	rows, err := h.pool.Query(r.Context(),
		"SELECT t.turn_seq, COALESCE(m.content, ''), COALESCE(t.completed_at, t.started_at) "+
			"FROM turns t JOIN LATERAL ("+
			"  SELECT content FROM messages WHERE parent_id = t.turn_id AND role = 'assistant' "+
			"  ORDER BY seq DESC LIMIT 1"+
			") m ON true "+
			"WHERE t.parent_id = $1 AND t.parent_type = 'session' AND t.status = 'completed' "+
			"AND COALESCE(m.content, '') <> '' ORDER BY t.turn_seq DESC LIMIT $2",
		core.SessionKeyFor("android", userID, "channel:"+userID), maxBriefings,
	)
	if err != nil {
		http.Error(w, "failed to read briefings", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var items []briefing
	for rows.Next() {
		var seq int
		var content string
		var publishedAt time.Time
		if err := rows.Scan(&seq, &content, &publishedAt); err != nil {
			http.Error(w, "failed to read briefings", http.StatusInternalServerError)
			return
		}
		items = append(items, briefing{
			TurnSeq:     seq,
			Title:       "Mission Control briefing",
			Summary:     summaryForBriefing(content),
			AudioPath:   "/android/briefings/" + strconv.Itoa(seq) + "/audio",
			PublishedAt: publishedAt,
		})
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "failed to read briefings", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(briefingResponse{Briefings: items})
}

func (h *Handler) handleBriefingAudio(w http.ResponseWriter, r *http.Request) {
	userID, err := h.authenticatedUser(r)
	if err != nil {
		http.Error(w, "invalid session token", http.StatusUnauthorized)
		return
	}
	turnSeq, err := strconv.Atoi(r.PathValue("turnSeq"))
	if err != nil || turnSeq < 1 {
		http.Error(w, "invalid briefing", http.StatusBadRequest)
		return
	}

	content, err := h.briefingContent(r.Context(), userID, turnSeq)
	if err != nil {
		http.Error(w, "briefing not found", http.StatusNotFound)
		return
	}
	spoken := speech.SanitizeForSpeech(content)
	if spoken == "" {
		http.Error(w, "briefing has no speakable content", http.StatusNotFound)
		return
	}
	audio, err := speech.SynthesizeOgg(r.Context(), spoken)
	if err != nil {
		http.Error(w, "failed to synthesize briefing", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "audio/ogg")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	_, _ = w.Write(audio)
}

func (h *Handler) briefingContent(ctx context.Context, userID string, turnSeq int) (string, error) {
	var content string
	err := h.pool.QueryRow(ctx,
		"SELECT m.content FROM turns t JOIN LATERAL ("+
			"  SELECT content FROM messages WHERE parent_id = t.turn_id AND role = 'assistant' "+
			"  ORDER BY seq DESC LIMIT 1"+
			") m ON true WHERE t.parent_id = $1 AND t.parent_type = 'session' "+
			"AND t.turn_seq = $2 AND t.status = 'completed'",
		core.SessionKeyFor("android", userID, "channel:"+userID), turnSeq,
	).Scan(&content)
	return content, err
}

func summaryForBriefing(content string) string {
	content = strings.Join(strings.Fields(content), " ")
	const maxRunes = 120
	runes := []rune(content)
	if len(runes) <= maxRunes {
		return content
	}
	return string(runes[:maxRunes-1]) + "…"
}
