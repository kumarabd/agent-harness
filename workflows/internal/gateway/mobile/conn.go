package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

const (
	writeWait       = 10 * time.Second
	pongWait        = 60 * time.Second
	pingEvery       = 25 * time.Second
	coldStartTurns  = 20 // a client with no cursor gets at most this many trailing turns
	catchupPageSize = 50 // bounded pagination per catchup() round-trip — first-party-plan.md §6:
	// the old explicit-resume path replayed everything after the cursor in
	// one unbounded query. catchup() now loops pages until it either hits
	// the running turn (the tail) or a short page (caught up).

	// keepAliveEvery — derived from wf.IdleTTL (not a separate hardcoded
	// number that has to be remembered to move in lockstep with it): a
	// KeepAlive needs to land comfortably before the coordinator's own idle
	// timer would otherwise fire. /3 gives real margin for one missed or
	// delayed signal without the coordinator exiting while this connection
	// is still live. Deliberately its own timer, not the WS ping's 25s
	// cadence — that interval is tuned for connection health, this one for
	// however long IdleTTL happens to be (5-15min in the real design; a
	// separate concern that shouldn't force this one's tuning along with it).
	keepAliveEvery = wf.IdleTTL / 3
)

// conn is one WebSocket connection = one device.
type conn struct {
	h  *Handler
	ws *websocket.Conn

	sessionKey string
	userID     string
	deviceID   string

	wakeCh chan struct{}
	// resumeCh carries "resume" frames from the reader goroutine to serve's
	// main loop. catchup() and every conn field it touches (sentThrough,
	// curTurnSeq, ...) must only ever run on the main loop — routing resume
	// requests through a channel instead of calling catchup() directly from
	// the reader goroutine is what makes that true (it used to call
	// catchup() inline, racing with the main loop's own wakeCh-triggered
	// catchup() on every one of those fields).
	resumeCh chan int

	writeMu sync.Mutex
	// deadCh is closed the first time a WS write fails, from whichever
	// goroutine hit it (send() is called from both the main loop and the
	// reader goroutine's inline error frames; writeMu already serializes
	// the actual socket write, this just tells serve's main loop to stop
	// promptly instead of continuing to loop against a dead socket).
	deadCh   chan struct{}
	deadOnce sync.Once

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
	presenceUpsert(ctx, c.h.pool, c.sessionKey, c.deviceID)
	// docs/components/gateway/first-party-plan.md's cross-replica presence —
	// KeepAliveSignalName's own doc comment. Harness-agnostic on purpose: the
	// coordinator has no idea this is a WebSocket, only that something wants
	// its idle timer held off. Sent once now (this connection may be the
	// thing that wakes an already-idled-out coordinator back up — intended,
	// not a cost to avoid) and again every keepAliveEvery for as long as the
	// connection lives (the ticker below); stops the moment this function
	// returns, with no corresponding "disconnect" signal needed at all.
	_ = c.h.ingestor.KeepAlive(ctx, c.sessionKey)
	// context.Background(), not ctx: by the time this defer runs, ctx (the
	// upgrade request's own context) may already be cancelled — a cleanup
	// write needs its own chance to land regardless. Not load-bearing either
	// way (a crash skips this defer entirely and the row just ages out via
	// presenceStaleAfterSeconds), but a clean disconnect should still clean
	// up promptly when it can.
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		presenceRemove(dctx, c.h.pool, c.sessionKey, c.deviceID)
	}()

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
	keepAlive := time.NewTicker(keepAliveEvery)
	defer keepAlive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-readErr:
			if !errors.Is(err, context.Canceled) && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("mobile: %s read: %v", c.sessionKey, err)
			}
			return
		case <-c.deadCh:
			return
		case <-c.wakeCh:
			c.catchup(ctx)
		case seq := <-c.resumeCh:
			c.sentThrough = seq
			c.curTurnSeq = -1
			c.catchup(ctx)
		case <-ping.C:
			c.writeMu.Lock()
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			err := c.ws.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
			// Presence heartbeat rides this same tick — no separate timer.
			// last_seen_at only needs to move roughly as often as the socket
			// itself proves alive.
			presenceUpsert(ctx, c.h.pool, c.sessionKey, c.deviceID)
		case <-keepAlive.C:
			// Its own, coarser timer — sized against wf.IdleTTL, not the WS
			// ping's connection-health cadence. Best-effort like everything
			// else here: a missed one just means the next tick catches up,
			// same self-healing posture as presenceUpsert.
			_ = c.h.ingestor.KeepAlive(ctx, c.sessionKey)
		}
	}
}

// send marshals and writes one frame. On a write failure it marks the
// connection dead (deadCh) so serve's main loop tears it down promptly
// instead of continuing to advance cursor state against a socket that isn't
// actually delivering anything any more — the client's own cursor (persisted
// only after it applies a frame locally) is the source of truth on
// reconnect regardless, so this is a promptness/observability fix, not a
// data-loss one.
func (c *conn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	err = c.ws.WriteMessage(websocket.TextMessage, b)
	c.writeMu.Unlock()
	if err != nil {
		c.deadOnce.Do(func() { close(c.deadCh) })
	}
	return err
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
		// first-party-plan.md §2: User is the human (the verified Clerk
		// sub), never the device — that was the bug (User used to carry
		// dev here, so speaker_id never actually held the human's identity
		// for a mobile message). DeviceID is separate, optional metadata.
		_, err := c.h.ingestor.Ingest(ctx, core.MessageEvent{
			Platform:          "mobile",
			ChannelID:         c.userID,
			User:              c.userID,
			DeviceID:          dev,
			Mode:              f.Mode,
			Content:           f.Text,
			PlatformMessageID: f.ClientMsgID,
			Discriminator:     "channel:" + c.userID,
		})
		if err != nil {
			c.send(errorFrame{Type: "error", Message: "failed to submit message"})
		}
	case "answer":
		c.answerUserInput(ctx, f)
	case "cancel":
		if err := core.CancelActiveTurn(ctx, c.h.pool, c.h.temporal, c.userID, c.sessionKey); err != nil {
			c.send(errorFrame{Type: "error", Message: "failed to cancel"})
		}
	case "resume":
		if f.AfterTurnSeq != nil {
			select {
			case c.resumeCh <- *f.AfterTurnSeq:
			case <-ctx.Done():
			}
		}
	case "pong", "ping":
		// keepalive handled by the WS control frames; ignore app-level ones
	default:
		c.send(errorFrame{Type: "error", Message: "unknown frame type: " + f.Type})
	}
}

// answerUserInput validates that f.RequestID actually belongs to this
// connection's OWN session before signaling it (core.AnswerUserInput) —
// first-party-plan.md §6: this used to look up the request by ID alone, with
// no check it belonged to the caller's session at all.
func (c *conn) answerUserInput(ctx context.Context, f inboundFrame) {
	if f.RequestID == "" {
		return
	}
	resp := types.UserInputResponse{RequestID: f.RequestID}
	if f.SelectedOptionID != "" {
		resp.SelectedOptionID = &f.SelectedOptionID
	}
	if f.FreeText != "" {
		resp.FreeText = &f.FreeText
	}
	err := core.AnswerUserInput(ctx, c.h.pool, c.h.temporal, c.sessionKey, resp)
	var already *core.AlreadyAnsweredError
	switch {
	case err == nil, errors.As(err, &already):
		// accepted, or already answered elsewhere (another device, the
		// 1-hour timeout) — idempotent either way, no error to the client.
	case errors.Is(err, core.ErrNotOwner):
		c.send(errorFrame{Type: "error", Message: "unknown request_id"})
	default:
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
	return trailingCursor(maxSeq, coldStartTurns)
}

// catchup replays every turn after c.sentThrough, paginated (catchupPageSize
// per round-trip — first-party-plan.md §6: this used to be one unbounded
// query on the explicit-resume path). It loops pages until it either hits
// the running turn (the tail — stop and wait for the next wake) or a
// short/empty page (genuinely caught up).
func (c *conn) catchup(ctx context.Context) {
	for {
		n, hitRunning := c.catchupPage(ctx)
		if hitRunning || n < catchupPageSize {
			return
		}
	}
}

func (c *conn) catchupPage(ctx context.Context) (n int, hitRunning bool) {
	rows, err := c.h.pool.Query(ctx,
		"SELECT turn_seq, turn_id, status, COALESCE(initiated_by, 'user') "+
			"FROM turns WHERE parent_id = $1 AND parent_type = 'session' AND turn_seq > $2 "+
			"ORDER BY turn_seq LIMIT $3",
		c.sessionKey, c.sentThrough, catchupPageSize,
	)
	if err != nil {
		return 0, false
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

		if isTerminal(t.status) {
			c.send(turnEndFrame{Type: "turn_end", TurnSeq: t.seq, Status: t.status})
			c.sentThrough = t.seq
			c.curTurnSeq = -1
		} else {
			c.emitAskUser(ctx, t.seq)
			return len(turns), true // running — this is the tail; wait for the next wake
		}
	}
	return len(turns), false
}

func (c *conn) emitMessages(ctx context.Context, turnSeq int, turnID string) {
	rows, err := c.h.pool.Query(ctx,
		"SELECT seq, role, COALESCE(content,''), COALESCE(speaker_id,''), COALESCE(client_msg_id,''), "+
			"COALESCE(client_device_id,'') FROM messages WHERE parent_id = $1 AND seq > $2 ORDER BY seq",
		turnID, c.sentMsgSeq,
	)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var role, content, speaker, cmid, devID string
		if rows.Scan(&seq, &role, &content, &speaker, &cmid, &devID) != nil {
			return
		}
		c.send(messageFrame{
			Type: "message", TurnSeq: turnSeq, Seq: seq, Role: role,
			Content: content, SpeakerID: speaker, DeviceID: devID, ClientMsgID: cmid,
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
		text, replace := deltaFor(c.lastCum, cum)
		c.send(deltaFrame{Type: "delta", TurnSeq: turnSeq, Seq: seq, Text: text, Replace: replace})
		c.lastCum = cum
		c.sentDelSeq = seq
	}
}

// emitStatus — a status ping delivered as a `delta`, not its own frame type.
// Treated as "a response without a request": it shares c.lastCum with
// emitDeltas, so when real streamed content eventually arrives it won't
// extend a status line's text, and deltaFor naturally emits replace:true —
// the exact mechanism already built for reconnect snapshots/provider
// backtracks, reused here for free. The client needs no separate status
// handling at all: it's speaking/rendering "Still working on this…" as one
// utterance, then the real answer as the next, through the one delta path.
// turn_status_pings/StatusPing themselves are unchanged and still shared
// with Discord's own (structurally different — post a channel message)
// delivery.
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
		dtext, replace := deltaFor(c.lastCum, text)
		c.send(deltaFrame{Type: "delta", TurnSeq: turnSeq, Seq: seq, Text: dtext, Replace: replace})
		c.lastCum = text
		c.sentStSeq = seq
	}
}

// emitAskUser is session-scoped (core.PendingInputFor), not turn-scoped —
// matching web/poll.go's own pending-input query. There is only ever one
// pending request per session at a time (a folded-in message cancels the
// in-flight one via CloseUserInput), so this is equivalent to the old
// turn-scoped query in practice and reuses the same shared reader mobile's
// answer path now goes through.
func (c *conn) emitAskUser(ctx context.Context, turnSeq int) {
	p, err := core.PendingInputFor(ctx, c.h.pool, c.sessionKey)
	if err != nil || p == nil || p.RequestID == c.askSent {
		return
	}
	c.send(askUserFrame{
		Type: "ask_user", TurnSeq: turnSeq, RequestID: p.RequestID, Kind: p.Kind,
		Prompt: p.Prompt, Options: json.RawMessage(p.Options), AllowFreeText: p.AllowFreeText,
	})
	c.askSent = p.RequestID
}
