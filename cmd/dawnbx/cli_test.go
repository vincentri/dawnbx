package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// captureStderr runs f with os.Stderr pointed at a pipe, so the warnings and
// per-id errors the client prints outside its writer can be read back.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	w.Close()
	os.Stderr = old
	return <-done
}

// inMinutes matches the countdown the EXPIRES column shows.
var inMinutes = regexp.MustCompile(`\bin \d+m\b`)

// deadURL is an address nothing listens on, for the "server is not running"
// path. The port is closed, so the dial is refused immediately.
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

// fakeAPI is a server that records what the client sent and answers from a
// routing table.
type fakeAPI struct {
	*httptest.Server
	seen []string
	key  string
}

func newFakeAPI(t *testing.T, key string, routes map[string]string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{key: key}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.seen = append(f.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(401)
			w.Write([]byte(`{"code":"unauthorized","message":"missing or wrong API key"}`))
			return
		}
		for route, reply := range routes {
			if r.URL.Path == route || (strings.HasSuffix(route, "*") && strings.HasPrefix(r.URL.Path, strings.TrimSuffix(route, "*"))) {
				w.Write([]byte(reply))
				return
			}
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"code":"not_found","message":"nope","hint":"dawnbx ls"}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) last() string {
	if len(f.seen) == 0 {
		return ""
	}
	return f.seen[len(f.seen)-1]
}

// TestConfigResolution: the environment wins, the installer's env file fills
// the gaps, and the loopback default is the last resort.
func TestConfigResolution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	t.Setenv("DAWNBX_URL", "")
	t.Setenv("DAWNBX_API_KEY", "")
	dir := filepath.Join(home, ".dawnbx")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "env")
	os.WriteFile(envFile, []byte("# a comment\nexport DAWNBX_URL=http://file:8080/\nDAWNBX_API_KEY=from-file\nexport OTHER=x\n"), 0o600)

	if c := newClient(); c.url != "http://file:8080" || c.key != "from-file" {
		t.Errorf("env file: %+v", c)
	}
	// A trailing slash is trimmed, or every path would double up.
	if c := newClient(); strings.HasSuffix(c.url, "/") {
		t.Errorf("url not trimmed: %q", c.url)
	}
	// Either variable alone is enough; the file fills in the other.
	t.Setenv("DAWNBX_URL", "https://env:8443")
	if c := newClient(); c.url != "https://env:8443" || c.key != "from-file" {
		t.Errorf("url from env, key from file: %+v", c)
	}
	t.Setenv("DAWNBX_API_KEY", "env-key")
	if c := newClient(); c.url != "https://env:8443" || c.key != "env-key" {
		t.Errorf("both from env: %+v", c)
	}
	// Without the file, the key stays empty and the url is the local default.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DAWNBX_URL", "")
	t.Setenv("DAWNBX_API_KEY", "")
	if c := newClient(); c.url != "http://127.0.0.1:8080" || c.key != "" {
		t.Errorf("defaults: %+v", c)
	}
	t.Setenv("DAWNBX_URL", "http://only-url:1/")
	if c := newClient(); c.url != "http://only-url:1" || c.key != "" {
		t.Errorf("url only: %+v", c)
	}
}

// TestSudoUserHome: under sudo the key of the user who ran the installer wins,
// not root's. A SUDO_USER nobody knows falls back to $HOME instead of failing.
func TestSudoUserHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAWNBX_URL", "")
	t.Setenv("DAWNBX_API_KEY", "")
	dir := filepath.Join(home, ".dawnbx")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "env"), []byte("export DAWNBX_API_KEY=root-key\n"), 0o600)

	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	// Whatever the invoking account has in its own home, root's file is not
	// the one that gets used.
	want := ""
	if b, err := os.ReadFile(filepath.Join(me.HomeDir, ".dawnbx", "env")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "="); ok && k == "DAWNBX_API_KEY" {
				want = v
			}
		}
	}
	t.Setenv("SUDO_USER", me.Username)
	if c := newClient(); c.key != want {
		t.Errorf("under sudo the key is %q, want the one in %s (%q)", c.key, me.HomeDir, want)
	}
	// An unknown SUDO_USER is ignored, not fatal.
	t.Setenv("SUDO_USER", "no-such-user-here")
	if c := newClient(); c.key != "root-key" {
		t.Errorf("unknown sudo user: %+v", c)
	}
	// And the resolved key is what a request carries.
	srv := newFakeAPI(t, "root-key", map[string]string{"/v1/version": `{"version":"1"}`})
	c := newClient()
	c.url = srv.URL
	var out strings.Builder
	if code, err := run(c, "version", nil, &out); err != nil || code != 0 {
		t.Errorf("version: %d %v", code, err)
	}
	if !strings.Contains(srv.last(), "Bearer root-key") {
		t.Errorf("request: %s", srv.last())
	}
}

// TestUsageAndArgumentErrors: every command that cannot do its job exits 2 and
// says why, so a script can tell a mistake from a failure.
func TestUsageAndArgumentErrors(t *testing.T) {
	srv := newFakeAPI(t, "k", map[string]string{"/v1/version": `{"version":"1"}`})
	c := &client{url: srv.URL, key: "k"}
	var out strings.Builder

	for _, cmd := range []string{"help", "-h", "--help"} {
		out.Reset()
		if code, err := run(c, cmd, nil, &out); err != nil || code != 0 || !strings.Contains(out.String(), "usage: dawnbx") {
			t.Errorf("%s: %d %v %q", cmd, code, err, out.String())
		}
	}
	code, err := run(c, "frobnicate", nil, &out)
	if code != 2 || err == nil || !strings.Contains(err.Error(), `unknown command "frobnicate"`) ||
		!strings.Contains(err.Error(), "usage: dawnbx") {
		t.Errorf("unknown: %d %v", code, err)
	}
	for _, c2 := range []struct {
		cmd  string
		args []string
		want string
	}{
		{"exec", nil, "usage: dawnbx exec <id> <cmd...>"},
		{"exec", []string{"sb-1"}, "usage: dawnbx exec <id> <cmd...>"},
		{"fork", nil, "usage: dawnbx fork <id> [n]"},
		{"fork", []string{"sb-1", "lots"}, `n must be a number, got "lots"`},
		{"kill", nil, "usage: dawnbx kill <id>"},
		{"rm", nil, "usage: dawnbx kill <id>"},
	} {
		if code, err := run(c, c2.cmd, c2.args, &out); code != 2 || err == nil || !strings.Contains(err.Error(), c2.want) {
			t.Errorf("%s %v: %d %v", c2.cmd, c2.args, code, err)
		}
	}
	// Bad flags are exit 2 with the message already printed by the flag
	// package, so the client does not repeat it.
	errText := captureStderr(t, func() {
		if code, err := run(c, "create", []string{"--imag", "x"}, &out); code != 2 || err != nil {
			t.Errorf("bad flag: %d %v", code, err)
		}
	})
	if !strings.Contains(errText, "imag") {
		t.Errorf("flag error not printed: %q", errText)
	}
	// -h is not a failure.
	errText = captureStderr(t, func() {
		out.Reset()
		if code, err := run(c, "create", []string{"-h"}, &out); code != 0 || err != nil {
			t.Errorf("create -h: %d %v", code, err)
		}
	})
	if !strings.Contains(errText, "-image") {
		t.Errorf("create usage: %q", errText)
	}
	if n := len(srv.seen); n != 0 {
		t.Errorf("argument errors still made %d requests", n)
	}
}

// TestLsColumns: the NODE column only earns its place once sandboxes run on
// more than one machine.
func TestLsColumns(t *testing.T) {
	created := "2026-01-01T00:00:00Z"
	soon := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	srv := newFakeAPI(t, "k", map[string]string{"/v1/sandboxes": `{"sandboxes":[
		{"id":"sb-a","status":"running","image":"python:3.12-slim","network":"internet","created":"` + created + `","expires_at":null,"parent":"sb-p","node":"w1","warnings":["x"]},
		{"id":"sb-b","status":"stopped","reason":"disk_full","image":"alpine","network":"none","created":"` + created + `","expires_at":null,"node":"w2"},
		{"id":"sb-c","status":"running","image":"alpine","network":"internet","created":"` + created + `","expires_at":"` + soon + `","node":"w1"}]}`})
	c := &client{url: srv.URL, key: "k"}
	var out strings.Builder
	if code, err := run(c, "list", nil, &out); err != nil || code != 0 {
		t.Fatalf("list: %d %v", code, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines: %q", out.String())
	}
	head := lines[0]
	for _, col := range []string{"ID", "STATUS", "IMAGE", "NETWORK", "PARENT", "AGE", "EXPIRES", "NODE"} {
		if !strings.Contains(head, col) {
			t.Errorf("header %q has no %s", head, col)
		}
	}
	if !strings.Contains(lines[1], "sb-p") || !strings.Contains(lines[2], "stopped (disk_full)") {
		t.Errorf("rows: %q", out.String())
	}
	// expires_at null reads as "never"; a real deadline counts down in minutes.
	if !strings.Contains(lines[1], "never") {
		t.Errorf("expiry column: %q", lines[1])
	}
	if !inMinutes.MatchString(lines[3]) {
		t.Errorf("expiry column: %q", lines[3])
	}
	if !strings.Contains(lines[1], "sb-p") || strings.Contains(lines[1], "w1 w1") {
		t.Errorf("parent column: %q", lines[1])
	}

	// One node: the column is dropped and a missing parent prints as a dash.
	srv2 := newFakeAPI(t, "k", map[string]string{"/v1/sandboxes": `{"sandboxes":[
		{"id":"sb-a","status":"running","image":"python:3.12-slim","network":"internet","created":"` + created + `","expires_at":null,"node":"w1"}]}`})
	out.Reset()
	if code, err := run(&client{url: srv2.URL, key: "k"}, "ls", nil, &out); err != nil || code != 0 {
		t.Fatalf("ls: %d %v", code, err)
	}
	if strings.Contains(out.String(), "NODE") || !strings.Contains(out.String(), "-") {
		t.Errorf("single node listing: %q", out.String())
	}
	// An empty list is still a table with a header.
	srv3 := newFakeAPI(t, "k", map[string]string{"/v1/sandboxes": `{"sandboxes":[]}`})
	out.Reset()
	if code, err := run(&client{url: srv3.URL, key: "k"}, "ls", nil, &out); err != nil || code != 0 || !strings.Contains(out.String(), "ID") {
		t.Errorf("empty list: %d %v %q", code, err, out.String())
	}
}

// TestExecAndFork: exit codes, stderr and the arguments the server receives.
func TestExecAndFork(t *testing.T) {
	srv := newFakeAPI(t, "k", map[string]string{
		"/v1/sandboxes/sb-1/exec": `{"exit_code":0,"stdout":"out\n","stderr":"warn\n"}`,
		"/v1/sandboxes/sb-1/fork": `{"sandboxes":[{"id":"sb-2"},{"id":"sb-3"}]}`,
		"/v1/sandboxes":           `{"id":"sb-new","warnings":["image ENTRYPOINT is not run"]}`,
	})
	c := &client{url: srv.URL, key: "k"}
	var out strings.Builder

	errText := captureStderr(t, func() {
		out.Reset()
		if code, err := run(c, "exec", []string{"sb-1", "sh", "-c", "echo hi"}, &out); err != nil || code != 0 {
			t.Errorf("exec: %d %v", code, err)
		}
	})
	if out.String() != "out\n" || errText != "warn\n" {
		t.Errorf("exec output %q stderr %q", out.String(), errText)
	}
	if !strings.Contains(srv.last(), `{"cmd":"sh -c echo hi"}`) {
		t.Errorf("exec body: %s", srv.last())
	}
	// A failing command is the script's exit code, not an error of the client.
	srv2 := newFakeAPI(t, "k", map[string]string{"/v1/sandboxes/sb-1/exec": `{"exit_code":42,"stdout":"","stderr":""}`})
	out.Reset()
	if code, err := run(&client{url: srv2.URL, key: "k"}, "exec", []string{"sb-1", "false"}, &out); err != nil || code != 42 {
		t.Errorf("exit code: %d %v", code, err)
	}

	out.Reset()
	if code, err := run(c, "fork", []string{"sb-1"}, &out); err != nil || code != 0 || out.String() != "sb-2\nsb-3\n" {
		t.Errorf("fork: %d %v %q", code, err, out.String())
	}
	if !strings.Contains(srv.last(), `{"count":1}`) {
		t.Errorf("fork body: %s", srv.last())
	}
	out.Reset()
	if code, err := run(c, "fork", []string{"sb-1", "3"}, &out); err != nil || code != 0 {
		t.Errorf("fork 3: %d %v", code, err)
	}
	if !strings.Contains(srv.last(), `{"count":3}`) {
		t.Errorf("fork body: %s", srv.last())
	}

	// Create prints warnings to stderr and the id to stdout.
	out.Reset()
	errText = captureStderr(t, func() {
		if code, err := run(c, "create", []string{"--image", "alpine:3", "--ttl", "forever", "--network", "none"}, &out); err != nil || code != 0 {
			t.Errorf("create: %d %v", code, err)
		}
	})
	if out.String() != "sb-new\n" {
		t.Errorf("create stdout %q", out.String())
	}
	if !strings.Contains(errText, "warning: image ENTRYPOINT") {
		t.Errorf("create warnings: %q", errText)
	}
	body := srv.last()
	for _, want := range []string{`"image":"alpine:3"`, `"ttl":null`, `"network":"none"`} {
		if !strings.Contains(body, want) {
			t.Errorf("create body %s has no %s", body, want)
		}
	}
	// A ttl of "1h" is sent as a string, and no flag means no key at all.
	captureStderr(t, func() {
		if code, err := run(c, "create", []string{"--ttl", "1h"}, &out); err != nil || code != 0 {
			t.Errorf("create ttl: %d %v", code, err)
		}
	})
	if !strings.Contains(srv.last(), `"ttl":"1h"`) {
		t.Errorf("ttl body: %s", srv.last())
	}
	captureStderr(t, func() {
		if code, err := run(c, "create", nil, &out); err != nil || code != 0 {
			t.Errorf("create: %d %v", code, err)
		}
	})
	if body := strings.TrimSpace(strings.SplitN(srv.last(), "Bearer k ", 2)[1]); body != "{}" {
		t.Errorf("bare create body %q, want no fields", body)
	}
}

// TestKillKeepsGoingAndReports: killing several sandboxes reports each failure
// and still tries the rest, and exits 1 if any of them failed.
func TestKillKeepsGoingAndReports(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/v1/sandboxes/sb-bad" {
			w.WriteHeader(404)
			w.Write([]byte(`{"code":"not_found","message":"gone","hint":"dawnbx ls"}`))
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	c := &client{url: srv.URL, key: "k"}
	var out strings.Builder
	errText := captureStderr(t, func() {
		if code, err := run(c, "kill", []string{"sb-ok", "sb-bad", "sb-ok2"}, &out); code != 1 || err != nil {
			t.Errorf("kill: %d %v", code, err)
		}
	})
	if !strings.Contains(errText, "sb-bad: not_found: gone") || !strings.Contains(errText, "hint: dawnbx ls") {
		t.Errorf("kill stderr %q", errText)
	}
	if len(paths) != 3 {
		t.Errorf("kill stopped at the first failure: %v", paths)
	}
}

// TestClientErrors: what the client says when the server is down, answers with
// something that is not the API's error shape, or refuses the key.
func TestClientErrors(t *testing.T) {
	var out strings.Builder
	c := &client{url: deadURL(t), key: "k"}
	code, err := run(c, "ls", nil, &out)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "cannot reach") ||
		!strings.Contains(err.Error(), "systemctl status dawnbx") {
		t.Errorf("unreachable: %d %v", code, err)
	}

	// A proxy that answers 502 with an HTML body still has to produce an error
	// the user can read, with the status in it.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer html.Close()
	err = (&client{url: html.URL, key: "k"}).do("GET", "/v1/sandboxes", nil, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "http_error") || !strings.Contains(err.Error(), "502") {
		t.Errorf("proxy error: %v", err)
	}

	// 401 with no key configured gets the hint that tells a newcomer what to do.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":"unauthorized","message":"missing or wrong API key","hint":"server hint"}`))
	}))
	defer srv.Close()
	err = (&client{url: srv.URL}).do("GET", "/v1/sandboxes", nil, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "--new-key") || strings.Contains(err.Error(), "server hint") {
		t.Errorf("missing key hint: %v", err)
	}
	// With a key set, the server's own hint is left alone.
	err = (&client{url: srv.URL, key: "wrong"}).do("GET", "/v1/sandboxes", nil, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "server hint") {
		t.Errorf("server hint: %v", err)
	}
	var ae *apiErr
	if !errors.As(err, &ae) || ae.Code != "unauthorized" || ae.Message == "" {
		t.Errorf("error type %T %v", err, err)
	}
	if got := (&apiErr{Code: "x", Message: "y"}).Error(); got != "x: y" {
		t.Errorf("apiErr without hint: %q", got)
	}
	if got := (&apiErr{Code: "x", Message: "y", Hint: "z"}).Error(); got != "x: y\nhint: z" {
		t.Errorf("apiErr with hint: %q", got)
	}
	// A 200 with a body the caller does not want is not an error.
	if err := (&client{url: srv2URL(t), key: "k"}).do("DELETE", "/v1/sandboxes/sb-1", nil, nil); err != nil {
		t.Errorf("delete: %v", err)
	}
}

func srv2URL(t *testing.T) string {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(s.Close)
	return s.URL
}

// TestVersionAndDoctor: the two commands that work when everything else fails.
func TestVersionAndDoctor(t *testing.T) {
	if _, err := os.Stat("/etc/dawnbx/install.json"); err == nil {
		t.Skip("running on a dawnbx server; doctor would run host checks")
	}
	srv := newFakeAPI(t, "k", map[string]string{
		"/v1/version": `{"version":"1.2.3","api":"v1"}`,
		"/v1/status":  `{"free_pct":42,"warm":2,"pool_size":2}`,
	})
	c := &client{url: srv.URL, key: "k"}
	var out strings.Builder
	if code, err := run(c, "version", nil, &out); err != nil || code != 0 ||
		!strings.Contains(out.String(), "client "+version) || !strings.Contains(out.String(), "server 1.2.3") {
		t.Errorf("version: %d %v %q", code, err, out.String())
	}
	// An unreachable server still prints both lines and fails, so a script can
	// read the client version from a broken box.
	c = &client{url: deadURL(t), key: "k"}
	out.Reset()
	if code, err := run(c, "version", nil, &out); err == nil || code != 0 || !strings.Contains(out.String(), "server unreachable") {
		t.Errorf("version offline: %d %v %q", code, err, out.String())
	}

	out.Reset()
	if code := doctor(c, &out); code != 1 || !strings.Contains(out.String(), "FAIL  server reachable at") ||
		!strings.Contains(out.String(), "systemctl restart dawnbx") {
		t.Errorf("doctor offline: %d %q", code, out.String())
	}
	if !strings.Contains(out.String(), "not on the server") {
		t.Errorf("doctor host checks: %q", out.String())
	}
	// Every API check passes.
	out.Reset()
	if code := doctor(&client{url: srv.URL, key: "k"}, &out); code != 0 {
		t.Errorf("doctor: %d %q", code, out.String())
	}
	for _, want := range []string{"ok    server reachable at", "ok    API key accepted", "ok    data disk 42% free", "ok    warm pool 2/2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor output %q has no %q", out.String(), want)
		}
	}
	// A full disk is a failure, a half-filled warm pool is only a warning.
	low := newFakeAPI(t, "k", map[string]string{
		"/v1/version": `{"version":"1"}`,
		"/v1/status":  `{"free_pct":9,"warm":0,"pool_size":2}`,
	})
	out.Reset()
	if code := doctor(&client{url: low.URL, key: "k"}, &out); code != 1 ||
		!strings.Contains(out.String(), "FAIL  data disk: 9% free") ||
		!strings.Contains(out.String(), "warn  warm pool: 0/2 ready") {
		t.Errorf("doctor unhealthy: %d %q", code, out.String())
	}
	// A key the server does not accept stops the checks there.
	noKey := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/version" {
			w.Write([]byte(`{"version":"1"}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"code":"unauthorized","message":"missing or wrong API key"}`))
	}))
	defer noKey.Close()
	out.Reset()
	if code := doctor(&client{url: noKey.URL, key: "k"}, &out); code != 1 ||
		!strings.Contains(out.String(), "FAIL  API key accepted: unauthorized: missing or wrong API key") {
		t.Errorf("doctor bad key: %d %q", code, out.String())
	}
	if strings.Contains(out.String(), "data disk") {
		t.Errorf("doctor kept going after the key was refused: %q", out.String())
	}
}

// TestAgoAndOr: the small formatters ls and the expiry column are built on.
func TestAgoAndOr(t *testing.T) {
	for _, c := range []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{59 * time.Second, "59s"},
		{90 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{90 * time.Minute, "1h30m"},
		{47 * time.Hour, "47h0m"},
		{72 * time.Hour, "3d"},
	} {
		if got := ago(c.in); got != c.want {
			t.Errorf("ago(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if or("", "-") != "-" || or("x", "-") != "x" {
		t.Error("or")
	}
}

// TestClientSendsJSON: bodies are JSON and the content type says so, so the
// server can reject a form post the same way it would reject a bad key.
func TestClientSendsJSON(t *testing.T) {
	var ct, method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct, method, path = r.Header.Get("Content-Type"), r.Method, r.URL.Path
		var v map[string]any
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		w.Write([]byte(`{"id":"sb-1"}`))
	}))
	defer srv.Close()
	var out strings.Builder
	if code, err := run(&client{url: srv.URL, key: "k"}, "create", nil, &out); err != nil || code != 0 {
		t.Fatalf("create: %d %v", code, err)
	}
	if ct != "application/json" || method != "POST" || path != "/v1/sandboxes" {
		t.Errorf("%s %s %s", method, path, ct)
	}
}
