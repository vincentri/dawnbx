package sandbox

import (
	"context"
	"io"
	"strings"
	"testing"

	"dawnbx/internal/store"
)

// Files go through exec inside the sandbox, never host paths. What matters to
// a caller is which error comes back: a missing file, a file too big to hand
// over, and a write that hit the disk cap are three different answers.
func TestReadFileErrors(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-read0001", nil)
	body := strings.Repeat("x", 64)
	m.RunExec = func(_ context.Context, id string, cmd []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		if id != s.ID {
			t.Errorf("read ran in %s", id)
		}
		if len(cmd) == 0 || cmd[0] != "cat" {
			t.Errorf("read command %v", cmd)
		}
		io.WriteString(stdout, body)
		return 0, nil
	}
	got, err := m.ReadFile(ctx, s.ID, "notes.txt")
	if err != nil || string(got) != body {
		t.Fatalf("read: %q %v", got, err)
	}

	m.RunExec = func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) { return 1, nil }
	if _, err := m.ReadFile(ctx, s.ID, "nope.txt"); err == nil || err.(*Error).Code != "file_not_found" {
		t.Errorf("missing file: %v", err)
	}

	// Exactly the cap: the caller must chunk instead of streaming 10 MB.
	bigFile := func(n int64) func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) {
		return func(_ context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
			stdout.Write(make([]byte, n))
			return 0, nil
		}
	}
	m.RunExec = bigFile(maxOutput)
	if _, err := m.ReadFile(ctx, s.ID, "big.bin"); err == nil || err.(*Error).Code != "file_too_large" {
		t.Errorf("10 MB file: %v", err)
	}
	// One byte under the cap still comes through.
	m.RunExec = bigFile(maxOutput - 1)
	if b, err := m.ReadFile(ctx, s.ID, "big.bin"); err != nil || len(b) != maxOutput-1 {
		t.Errorf("just under the cap: %d bytes, %v", len(b), err)
	}

	stopped := mk(t, m, "sb-read0002", func(x *store.Meta) { x.Status = StatusStopped })
	if _, err := m.ReadFile(ctx, stopped.ID, "x"); err == nil || err.(*Error).Code != "sandbox_stopped" {
		t.Errorf("read from a stopped sandbox: %v", err)
	}
}

// A write that hit the quota is the user's problem to fix by deleting files,
// and must not be reported as a generic failure.
func TestWriteFileErrors(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-write001", nil)

	var wrote string
	m.RunExec = func(_ context.Context, _ string, cmd []string, stdin io.Reader, _ io.Writer, _ io.Writer) (int, error) {
		b, _ := io.ReadAll(stdin)
		wrote = string(b)
		if len(cmd) == 0 || cmd[0] != "sh" {
			t.Errorf("write command %v", cmd)
		}
		return 0, nil
	}
	if err := m.WriteFile(ctx, s.ID, "a/b.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	if wrote != "hello" {
		t.Errorf("body not forwarded: %q", wrote)
	}

	for _, tc := range []struct{ msg, code string }{
		{"project quota exceeded", "disk_limit"},
		{"No space left on device", "disk_limit"},
		{"permission denied", "write_failed"},
	} {
		m.RunExec = func(_ context.Context, _ string, _ []string, _ io.Reader, _ io.Writer, stderr io.Writer) (int, error) {
			io.WriteString(stderr, tc.msg+"\n")
			return 1, nil
		}
		err := m.WriteFile(ctx, s.ID, "a/b.txt", strings.NewReader("x"))
		e, ok := err.(*Error)
		if !ok || e.Code != tc.code {
			t.Errorf("%q: want %s, got %v", tc.msg, tc.code, err)
		}
	}
}

// A background exec hands back the pid and the log path, and the command must
// be nohup'd with its output redirected there.
func TestExecBackground(t *testing.T) {
	m, _ := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-bg00001", nil)
	var line string
	m.RunExec = func(_ context.Context, _ string, cmd []string, _ io.Reader, stdout, _ io.Writer) (int, error) {
		line = strings.Join(cmd, " ")
		io.WriteString(stdout, "4242\n")
		return 0, nil
	}
	r, err := m.Exec(ctx, s.ID, ExecReq{Cmd: "sleep 100 &", Background: true, Env: map[string]string{"A": "1"}})
	if err != nil || r.PID != 4242 {
		t.Fatalf("background exec: %+v %v", r, err)
	}
	if !strings.Contains(r.Log, "dawnbx-bg-") {
		t.Errorf("log path %q", r.Log)
	}
	if !strings.Contains(line, "nohup") || !strings.Contains(line, r.Log) || !strings.Contains(line, "A=1") {
		t.Errorf("background command %q", line)
	}
	// An empty command is a client error, not an empty shell.
	if _, err := m.Exec(ctx, s.ID, ExecReq{}); err == nil || err.(*Error).Code != "invalid_request" {
		t.Errorf("empty cmd: %v", err)
	}
}
