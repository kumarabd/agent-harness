package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"agent-harness/workflows/internal/gateway/core"
	wf "agent-harness/workflows/internal/workflow"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingEvery      = 25 * time.Second
	coldStartTurns = 20 // a client with no cursor gets at most this many trailing turns
)

// conn is one WebSocket connection = one device.
type conn struct {
	h  *Handler
	ws *websocket.Conn

	sessionKey string
	userID     string
	deviceID   string

	wakeCh  chan struct{}
	writeMu sync.Mutex

	// Resume cursor — advanced as frames are emitted, never persisted server
	// side (the client sends its own on reconnect).
	sentThrough int    // highest turn_seq fully emitted (turn_end sent)
	curTurnSeq  int    // the turn currently being tailed (-1 = none)
	curStarted  bool   // turn_start emitted for curTurnSeq
	sentMsgSeq  int    // highest messages.seq emitted for curTurnSeq
	sentDelSeq  int    // highest turn_deliveries.seq emitted for curTurnSeq
	sentStSeq   int    // highest turn_status_pings.seq emitted for curTurnSeq
	lastCum     string // last cumulative streamed content for curTurnSeq
	askSent     string // request_id of the ask_user already surfaced for curTurnSeq
}

func (c *conn) notify() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

func (c *conn) serve(ctx context.Context) {
	defer c.ws.Close()

	// --- first frame must be auth ---
	c.ws.SetReadLimit(1 << 16)
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	var first inboundFrame
	if err := c.ws.ReadJSON(&first); err != nil || first.Type != "auth" || first.Token == "" {
		c.send(errorFrame{Type: "error", Message: "first frame must be {type:auth, token}"})
		return
	}
	sub, err := clerkVerify(ctx, c.h, first.Token)
	if err != nil {
		c.send(errorFrame{Type: "error", Message: "invalid token"})
		return
	}
	c.userID = sub
	c.deviceID = first.DeviceID
	c.sessionKey = core.SessionKeyFor("mobile", c.userID, "channel:"+c.userID)

	// resume cursor from the auth frame, else cold start
	if first.AfterTurnSeq != nil {
		c.sentThrough = *first.AfterTurnSeq
	} else {
		c.sentThrough = c.coldStartCursor(ctx)
	}

	c.h.hub.add(c)
	defer c.h.hub.remove(c)

	c.ws.SetPongHandler(func(string) error {
		_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// --- reader goroutine: inbound frames ---
	readErr := make(chan error, 1)
	go func() {
		for {
			var f inboundFrame
			if err := c.ws.ReadJSON(&f); err != nil {
				readErr <- err
				return
			}
			_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
			c.handleInbound(ctx, f)
		}
	}()

	// initial replay
	c.catchup(ctx)
	c.send(resumedFrame{Type: "resumed", ThroughTurnSeq: c.sentThrough})

	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-readErr:
			if !errors.Is(err, context.Canceled) && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("mobile: %s read: %v", c.sessionKey, err)
			}
			return
		case <-c.wakeCh:
			c.catchup(ctx)
		case <-ping.C:
			c.writeMu.Lock()
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (c *conn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	_ = c.ws.WriteMessage(websocket.TextMessage, b)
}

// --- inbound ---

func (c *conn) handleInbound(ctx context.Context, f inboundFrame) {
	switch f.Type {
	case "message":
		if f.Text == "" || f.ClientMsgID == "" {
			c.send(errorFrame{Type: "error", Message: "message needs text and client_msg_id"})
			return
		}
		dev := f.DeviceID
		if dev == "" {
			dev = c.deviceID
		}
		_, err := c.h.ingestor.Ingest(ctx, core.MessageEvent{
			Platform:          "mobile",
			ChannelID:         c.userID,
			User:              dev,
			Content:           f.Text,
			PlatformMessageID: f.ClientMsgID,
			Discriminator:     "channel:" + c.userID,
		})
		if err != nil {
			c.send(errorFrame{Type: "error", Message: "failed to submit message"})
		}
	case "answer":
		c.answerUserInput(ctx, f)
	case "resume":
		if f.AfterTurnSeq != nil {
			c.sentThrough = *f.AfterTurnSeq
			c.curTurnSeq = -1
			c.catchup(ctx)
		}
	case "pong", "ping":
		// keepalive handled by the WS control frames; ignore app-level ones
	default:
		c.send(errorFrame{Type: "error", Message: "unknown frame type: " + f.Type})
	}
}

func (c *conn) answerUserInput(ctx context.Context, f inboundFrame) {
	if f.RequestID == "" {
		return
	}
	var wfID string
	err := c.h.pool.QueryRow(ctx,
		"SELECT workflow_id FROM user_input_requests WHERE request_id = $1", f.RequestID,
	).Scan(&wfID)
	if err != nil || wfID == "" {
		c.send(errorFrame{Type: "error", Message: "unknown request_id"})
		return
	}
	resp := map[string]any{"request_id": f.RequestID}
	if f.SelectedOptionID != "" {
		resp["selected_option_id"] = f.SelectedOptionID
	}
	if f.FreeText != "" {
		resp["free_text"] = f.FreeText
	}
	if err := c.h.temporal.SignalWorkflow(ctx, wfID, "", wf.UserInputResponseSignalName, resp); err != nil {
		c.send(errorFrame{Type: "error", Message: "failed to deliver answer"})
	}
}

// --- outbound: tail the session ---

func (c *conn) coldStartCursor(ctx context.Context) int {
	var maxSeq *int
	_ = c.h.pool.QueryRow(ctx,
		"SELECT max(turn_seq) FROM turns WHERE parent_id = $1 AND parent_type = 'session'",
		c.sessionKey,
	).Scan(&maxSeq)
	if maxSeq == nil {
		return 0
	}
	s := *maxSeq - coldStartTurns
	if s < 0 {
		s = 0
	}
	return s
}

func (c *conn) catchup(ctx context.Context) {
	rows, err := c.h.pool.Query(ctx,
		"SELECT turn_seq, turn_id, status, COALESCE(initiated_by, 'user') "+
			"FROM turns WHERE parent_id = $1 AND parent_type = 'session' AND turn_seq > $2 "+
			"ORDER BY turn_seq",
		c.sessionKey, c.sentThrough,
	)
	if err != nil {
		return
	}
	type turnRow struct {
		seq         int
		id          string
		status      string
		initiatedBy string
	}
	var turns []turnRow
	for rows.Next() {
		var t turnRow
		if rows.Scan(&t.seq, &t.id, &t.status, &t.initiatedBy) == nil {
			turns = append(turns, t)
		}
	}
	rows.Close()

	for _, t := range turns {
		if t.seq != c.curTurnSeq {
			c.curTurnSeq = t.seq
			c.curStarted = false
			c.sentMsgSeq = -1
			c.sentDelSeq = 0
			c.sentStSeq = 0
			c.lastCum = ""
			c.askSent = ""
		}
		if !c.curStarted {
			c.send(turnStartFrame{Type: "turn_start", TurnSeq: t.seq, TurnID: t.id, InitiatedBy: t.initiatedBy})
			c.curStarted = true
		}
		c.emitMessages(ctx, t.seq, t.id)
		c.emitDeltas(ctx, t.seq, t.id)
		c.emitStatus(ctx, t.seq, t.id)

		terminal := t.status == "completed" || t.status == "failed" || t.status == "cancelled"
		if terminal {
			c.send(turnEndFrame{Type: "turn_end", TurnSeq: t.seq, Status: t.status})
			c.sentThrough = t.seq
			c.curTurnSeq = -1
		} else {
			c.emitAskUser(ctx, t.seq, t.id)
			return // running — this is the tail; wait for the next wake
		}
	}
}

func (c *conn) emitMessages(ctx context.Context, turnSeq int, turnID string) {
	rows, err := c.h.pool.Query(ctx,
		"SELECT seq, role, COALESCE(content,''), COALESCE(speaker_id,''), COALESCE(client_msg_id,'') "+
			"FROM messages WHERE parent_id = $1 AND seq > $2 ORDER BY seq",
		turnID, c.sentMsgSeq,
	)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var role, content, speaker, cmid string
		if rows.Scan(&seq, &role, &content, &speaker, &cmid) != nil {
			return
		}
		c.send(messageFrame{
			Type: "message", TurnSeq: turnSeq, Seq: seq, Role: role,
			Content: content, SpeakerID: speaker, ClientMsgID: cmid,
		})
		c.sentMsgSeq = seq
	}
}

func (c *conn) emitDeltas(ctx context.Context, turnSeq int, turnID string) {
	rows, err := c.h.pool.Query(ctx,
		"SELECT seq, content FROM turn_deliveries WHERE turn_id = $1 AND seq > $2 ORDER BY seq",
		turnID, c.sentDelSeq,
	)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var cum string
		if rows.Scan(&seq, &cum) != nil {
			return
		}
		var f deltaFrame
		if c.lastCum == "" || !strings.HasPrefix(cum, c.lastCum) {
			// first chunk after (re)connect, or a provider backtrack → snapshot
			f = deltaFrame{Type: "delta", TurnSeq: turnSeq, Seq: seq, Text: cum, Replace: true}
		} else {
			f = deltaFrame{Type: "delta", TurnSeq: turnSeq, Seq: seq, Text: cum[len(c.lastCum):]}
		}
		c.send(f)
		c.lastCum = cum
		c.sentDelSeq = seq
	}
}

func (c *conn) emitStatus(ctx context.Context, turnSeq int, turnID string) {
	rows, err := c.h.pool.Query(ctx,
		"SELECT seq, content FROM turn_status_pings WHERE turn_id = $1 AND seq > $2 ORDER BY seq",
		turnID, c.sentStSeq,
	)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var text string
		if rows.Scan(&seq, &text) != nil {
			return
		}
		c.send(statusFrame{Type: "status", TurnSeq: turnSeq, Text: text})
		c.sentStSeq = seq
	}
}

func (c *conn) emitAskUser(ctx context.Context, turnSeq int, turnID string) {
	var rid, kind, prompt string
	var opts []byte
	var free bool
	err := c.h.pool.QueryRow(ctx,
		"SELECT request_id, kind, prompt, options, allow_free_text FROM user_input_requests "+
			"WHERE turn_id = $1 AND status = 'pending' ORDER BY created_at DESC LIMIT 1",
		turnID,
	).Scan(&rid, &kind, &prompt, &opts, &free)
	if err != nil || rid == "" || rid == c.askSent {
		return
	}
	c.send(askUserFrame{
		Type: "ask_user", TurnSeq: turnSeq, RequestID: rid, Kind: kind,
		Prompt: prompt, Options: json.RawMessage(opts), AllowFreeText: free,
	})
	c.askSent = rid
}
