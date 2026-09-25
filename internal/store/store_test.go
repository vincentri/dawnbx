package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	root := t.TempDir()
	if _, err := Open(root); err == nil {
		t.Fatal("Open without marker: want error")
	}
	os.WriteFile(filepath.Join(root, Marker), []byte("x"), 0o644)
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		NewID(): true, "sb-abc123": true, "sb-ab_123": false, "sb-ABC123": false,
		"sb-abc": false, "../etc": false, "sb-abc123/..": false,
	} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) = %v", id, !want)
		}
	}
}

func TestRoundTripAndLayout(t *testing.T) {
	s := newStore(t)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	m := Meta{ID: "sb-test01", Image: "python:3.12-slim", Created: time.Now().UTC().Truncate(time.Second), ExpiresAt: &exp}
	if err := s.Create(m); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s.WS(m.ID)); err != nil || !fi.IsDir() {
		t.Fatal("ws/ not created")
	}
	if _, err := os.Stat(filepath.Join(s.WS(m.ID), "meta.json")); err == nil {
		t.Fatal("meta.json must not be inside ws/ (agent-visible)")
	}
	got, err := s.ReadMeta(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != Version || got.Image != m.Image || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("got %+v", got)
	}
	if _, err := s.ReadMeta("sb-nothere"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Create(Meta{ID: "sb_bad"}); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("bad id: %v", err)
	}
}

// A crash mid-write leaves a partial tmp file next to meta.json; reads must
// still see the last complete write, and the next write must succeed.
func TestCrashMidWrite(t *testing.T) {
	s := newStore(t)
	m := Meta{ID: "sb-crash1", Image: "a"}
	if err := s.Create(m); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(s.Dir(m.ID), "meta.json.tmp123"), []byte(`{"v":1,"id":"sb-cr`), 0o644)
	if got, err := s.ReadMeta(m.ID); err != nil || got.Image != "a" {
		t.Fatalf("after crash: %+v %v", got, err)
	}
	m.Image = "b"
	if err := s.WriteMeta(m); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadMeta(m.ID); got.Image != "b" {
		t.Fatalf("after rewrite: %+v", got)
	}
}

func TestNewerVersionSkipped(t *testing.T) {
	s := newStore(t)
	os.MkdirAll(s.WS("sb-future1"), 0o755)
	os.WriteFile(filepath.Join(s.Dir("sb-future1"), "meta.json"), []byte(`{"v":2,"id":"sb-future1"}`), 0o644)
	if _, err := s.ReadMeta("sb-future1"); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("want ErrUnknownVersion, got %v", err)
	}
}
