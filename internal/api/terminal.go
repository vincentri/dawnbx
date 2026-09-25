package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/tools/remotecommand"

	"dawnbx/internal/sandbox"
)

// Browsers cannot set headers on a WebSocket, so the key may also arrive as
// the subprotocol "bearer.<key>" next to "dawnbx" (the one echoed back).
// Default CheckOrigin rejects cross-site pages; tools that send no Origin pass.
var upgrader = websocket.Upgrader{Subprotocols: []string{"dawnbx"}}

// terminal bridges a WebSocket to a TTY shell in the sandbox.
// Client -> server: binary = keystrokes, text = {"cols":N,"rows":N}.
// Server -> client: binary = terminal output; the close reason carries any error.
func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the HTTP error
	}
	defer c.Close()
	c.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	in, inW := io.Pipe()
	sizes := make(termSizes, 4)
	go func() {
		defer cancel()
		defer inW.Close()
		defer close(sizes)
		for {
			t, b, err := c.ReadMessage()
			if err != nil {
				return
			}
			if t == websocket.BinaryMessage {
				if _, err := inW.Write(b); err != nil {
					return
				}
				continue
			}
			var sz struct{ Cols, Rows uint16 }
			if json.Unmarshal(b, &sz) == nil && sz.Cols > 0 && sz.Rows > 0 {
				select {
				case sizes <- remotecommand.TerminalSize{Width: sz.Cols, Height: sz.Rows}:
				default: // shell not reading resizes yet; drop, the next one wins
				}
			}
		}
	}()

	out := &wsWriter{c: c}
	err = s.M.Terminal(ctx, r.PathValue("id"), in, out, sizes)
	code, reason := websocket.CloseNormalClosure, "shell exited"
	var e *sandbox.Error
	if errors.As(err, &e) {
		code, reason = 4000+e.Status, e.Code+": "+e.Message
	} else if err != nil && ctx.Err() == nil {
		code, reason = websocket.CloseInternalServerErr, err.Error()
	}
	if len(reason) > 120 { // close frames cap the reason at 123 bytes
		reason = reason[:120]
	}
	out.mu.Lock()
	c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
	out.mu.Unlock()
}

type termSizes chan remotecommand.TerminalSize

func (s termSizes) Next() *remotecommand.TerminalSize {
	t, ok := <-s
	if !ok {
		return nil
	}
	return &t
}

type wsWriter struct {
	mu sync.Mutex
	c  *websocket.Conn
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.c.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
