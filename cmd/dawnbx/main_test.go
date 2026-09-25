package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			w.Write([]byte(`{"code":"unauthorized","message":"missing or wrong API key"}`))
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		raw, _ := json.Marshal(b)
		bodies = append(bodies, r.Method+" "+r.URL.Path+" "+string(raw))
		switch {
		case r.URL.Path == "/v1/sandboxes" && r.Method == "GET":
			w.Write([]byte(`{"sandboxes":[{"id":"sb-aaaaaaaaaa","status":"stopped","reason":"disk_full","image":"python:3.12-slim","network":"none","created":"2026-01-01T00:00:00Z","expires_at":null}]}`))
		case r.URL.Path == "/v1/sandboxes":
			w.Write([]byte(`{"id":"sb-bbbbbbbbbb"}`))
		case strings.HasSuffix(r.URL.Path, "/exec"):
			w.Write([]byte(`{"exit_code":3,"stdout":"hi\n","stderr":""}`))
		case r.Method == "DELETE":
			w.WriteHeader(404)
			w.Write([]byte(`{"code":"not_found","message":"gone","hint":"dawnbx ls shows live sandboxes"}`))
		}
	}))
	defer srv.Close()
	c := &client{url: srv.URL, key: "k"}
	var out strings.Builder

	if code, err := run(c, "ls", nil, &out); err != nil || code != 0 || !strings.Contains(out.String(), "stopped (disk_full)") || !strings.Contains(out.String(), "never") {
		t.Errorf("ls: %d %v %q", code, err, out.String())
	}
	out.Reset()
	if code, err := run(c, "create", []string{"--ttl", "forever", "--network", "none"}, &out); err != nil || code != 0 || out.String() != "sb-bbbbbbbbbb\n" {
		t.Errorf("create: %d %v %q", code, err, out.String())
	}
	if last := bodies[len(bodies)-1]; !strings.Contains(last, `"ttl":null`) || !strings.Contains(last, `"network":"none"`) {
		t.Errorf("create body: %s", last)
	}
	out.Reset()
	if code, err := run(c, "exec", []string{"sb-bbbbbbbbbb", "echo", "hi;", "exit", "3"}, &out); err != nil || code != 3 || out.String() != "hi\n" {
		t.Errorf("exec: %d %v %q", code, err, out.String())
	}
	if last := bodies[len(bodies)-1]; !strings.Contains(last, `"cmd":"echo hi; exit 3"`) {
		t.Errorf("exec body: %s", last)
	}
	if code, _ := run(c, "kill", []string{"sb-cccccccccc"}, &out); code != 1 {
		t.Errorf("kill of missing sandbox should fail, got %d", code)
	}
	if _, err := run(&client{url: srv.URL}, "ls", nil, &out); err == nil || !strings.Contains(err.Error(), "--new-key") {
		t.Errorf("missing key hint: %v", err)
	}
}

func TestEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAWNBX_URL", "")
	t.Setenv("DAWNBX_API_KEY", "")
	os.MkdirAll(filepath.Join(home, ".dawnbx"), 0o700)
	os.WriteFile(filepath.Join(home, ".dawnbx", "env"), []byte("export DAWNBX_URL=http://x:1/\nexport DAWNBX_API_KEY=dawnbx_k\n"), 0o600)
	if c := newClient(); c.url != "http://x:1" || c.key != "dawnbx_k" {
		t.Errorf("env file: %+v", c)
	}
	t.Setenv("DAWNBX_API_KEY", "fromenv")
	if c := newClient(); c.key != "fromenv" {
		t.Errorf("env var should win: %+v", c)
	}
}
