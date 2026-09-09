package mobile

// A live smoke test against a deployed gateway — skipped unless MOBILE_SMOKE_URL
// is set (e.g. ws://localhost:18090 behind a port-forward). It exercises the
// parts that don't need a real Clerk token: the WS upgrade, the first-frame-must-
// be-auth gate, and token rejection. The authenticated stream path is covered by
// the live scenario suite / the app itself.
//
//	MOBILE_SMOKE_URL=ws://localhost:18090 go test ./internal/gateway/mobile/ -run TestLiveSmoke -v

import (
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestLiveSmoke(t *testing.T) {
	base := os.Getenv("MOBILE_SMOKE_URL")
	if base == "" {
		t.Skip("set MOBILE_SMOKE_URL to run (e.g. ws://localhost:18090)")
	}

	dial := func(t *testing.T) *websocket.Conn {
		t.Helper()
		d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
		c, resp, err := d.Dial(base+"/ws", nil)
		if err != nil {
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			t.Fatalf("dial %s/ws: %v (http %d)", base, err, code)
		}
		return c
	}

	readErr := func(t *testing.T, c *websocket.Conn) errorFrame {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		var f errorFrame
		if err := c.ReadJSON(&f); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if f.Type != "error" {
			t.Fatalf("want error frame, got %q", f.Type)
		}
		return f
	}

	t.Run("upgrade succeeds", func(t *testing.T) {
		c := dial(t)
		c.Close()
	})

	t.Run("first frame must be auth", func(t *testing.T) {
		c := dial(t)
		defer c.Close()
		if err := c.WriteJSON(map[string]any{"type": "message", "text": "hi"}); err != nil {
			t.Fatalf("write: %v", err)
		}
		f := readErr(t, c)
		if f.Message == "" {
			t.Fatalf("empty error message")
		}
		t.Logf("rejected non-auth first frame: %q", f.Message)
	})

	t.Run("bad token rejected", func(t *testing.T) {
		c := dial(t)
		defer c.Close()
		if err := c.WriteJSON(map[string]any{"type": "auth", "token": "not-a-real-jwt"}); err != nil {
			t.Fatalf("write: %v", err)
		}
		f := readErr(t, c)
		if f.Message != "invalid token" {
			t.Fatalf("want %q, got %q", "invalid token", f.Message)
		}
		t.Logf("rejected bad token: %q", f.Message)
	})
}
