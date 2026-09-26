package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/tools/remotecommand"

	"dawnbx/internal/sandbox"
	"dawnbx/internal/store"
)

// termServer starts the API with a fake shell and returns a dialler for
// /v1/sandboxes/<id>/terminal.
func termServer(t *testing.T, shell func(in io.Reader, out io.Writer, sizes remotecommand.TerminalSizeQueue) error) (*sandbox.Manager, *store.Store, func(id string) *websocket.Conn) {
	t.Helper()
	m, st := testManager(t)
	id := store.NewID()
	st.Create(store.Meta{ID: id, Image: "x", Created: m.Now(), Status: sandbox.StatusRunning})
	m.RunTTY = func(_ context.Context, _ string, _ []string, in io.Reader, out io.Writer, sizes remotecommand.TerminalSizeQueue) (int, error) {
		return 0, shell(in, out, sizes)
	}
	srv := httptest.NewServer((&Server{M: m, Auth: testDB(t)}).Handler())
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/sandboxes/" + id + "/terminal"
	return m, st, func(id string) *websocket.Conn {
		t.Helper()
		d := websocket.Dialer{Subprotocols: []string{"dawnbx", "bearer.dawnbx_good"}}
		c, res, err := d.Dial(url+id, http.Header{"Origin": []string{srv.URL}})
		if err != nil {
			if res != nil {
				t.Fatalf("dial: %v (%d)", err, res.StatusCode)
			}
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
}

// closeFrame drains the session and returns the code and reason of the close
// the server sent last.
func closeFrame(t *testing.T, c *websocket.Conn) (int, string) {
	t.Helper()
	for {
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("close: %v", err)
		}
		return ce.Code, ce.Text
	}
}

// TestTerminalReportsSandboxErrors: a shell that cannot start is told to the
// client as a close code, not as a silent empty session.
func TestTerminalReportsSandboxErrors(t *testing.T) {
	_, _, dial := termServer(t, func(io.Reader, io.Writer, remotecommand.TerminalSizeQueue) error {
		return &sandbox.Error{Status: 404, Code: "not_found", Message: "sandbox is gone"}
	})
	// 4000 + the API status, so a client can tell a 404 from a 409.
	code, reason := closeFrame(t, dial(""))
	if code != 4404 {
		t.Errorf("close code %d, want 4404", code)
	}
	if !strings.HasPrefix(reason, "not_found: ") {
		t.Errorf("close reason %q", reason)
	}

	// An error the API did not classify is an internal error, and the reason
	// is cut to what a close frame can carry.
	_, _, dial = termServer(t, func(io.Reader, io.Writer, remotecommand.TerminalSizeQueue) error {
		return errors.New(strings.Repeat("x", 300))
	})
	code, reason = closeFrame(t, dial(""))
	if code != websocket.CloseInternalServerErr {
		t.Errorf("close code %d, want %d", code, websocket.CloseInternalServerErr)
	}
	if len(reason) != 120 {
		t.Errorf("close reason is %d bytes, want 120", len(reason))
	}
}

// TestTerminalSurvivesAResizeFlood: a client that sends resizes the shell never
// asks for must not wedge the input pump; the shell still gets its keystrokes
// and the session ends normally.
func TestTerminalSurvivesAResizeFlood(t *testing.T) {
	var keys int
	_, _, dial := termServer(t, func(in io.Reader, out io.Writer, _ remotecommand.TerminalSizeQueue) error {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			if sc.Text() == "exit" {
				return nil
			}
			keys++
		}
		return nil
	})
	c := dial("")
	for i := 0; i < 8; i++ {
		if err := c.WriteMessage(websocket.TextMessage, []byte(`{"cols":80,"rows":24}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("hi\nexit\n")); err != nil {
		t.Fatal(err)
	}
	if code, reason := closeFrame(t, c); code != websocket.CloseNormalClosure || reason != "shell exited" {
		t.Errorf("close %d %q", code, reason)
	}
	if keys != 1 {
		t.Errorf("shell saw %d lines, want 1", keys)
	}
}

// TestTerminalIgnoresRubbishFrames: keystrokes and resizes share one socket, so
// a malformed control frame is dropped instead of ending the session.
func TestTerminalIgnoresRubbishFrames(t *testing.T) {
	var seen string
	_, _, dial := termServer(t, func(in io.Reader, out io.Writer, sizes remotecommand.TerminalSizeQueue) error {
		sz := sizes.Next()
		fmt.Fprintf(out, "%dx%d\n", sz.Width, sz.Height)
		sc := bufio.NewScanner(in)
		for sc.Scan() && sc.Text() != "exit" {
			seen += sc.Text() + ";"
		}
		return nil
	})
	c := dial("")
	c.WriteMessage(websocket.TextMessage, []byte(`{"cols":0,"rows":0}`))    // ignored
	c.WriteMessage(websocket.TextMessage, []byte(`not json`))               // ignored
	c.WriteMessage(websocket.TextMessage, []byte(`{"cols":100,"rows":50}`)) // taken
	c.WriteMessage(websocket.BinaryMessage, []byte("a\nexit\n"))
	var out string
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			break
		}
		out += string(b)
	}
	if out != "100x50\n" || seen != "a;" {
		t.Errorf("out %q keys %q", out, seen)
	}
}

// TestTermSizesEndOfStream: a closed queue means "no more sizes", and the
// remote exec stream needs nil rather than a zero-sized one.
func TestTermSizesEndOfStream(t *testing.T) {
	s := make(termSizes, 1)
	s <- remotecommand.TerminalSize{Width: 3, Height: 4}
	if got := s.Next(); got == nil || got.Width != 3 || got.Height != 4 {
		t.Fatalf("first size %+v", got)
	}
	close(s)
	if got := s.Next(); got != nil {
		t.Errorf("size after close: %+v, want nil", got)
	}
	if got := s.Next(); got != nil {
		t.Errorf("second read after close: %+v, want nil", got)
	}
}
