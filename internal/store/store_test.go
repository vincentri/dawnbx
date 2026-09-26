package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
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

// IDs is the reaper's view of the data dir: it must skip anything that is
// not a sandbox dir, so a half-finished create or a stray file never becomes
// a phantom sandbox.
func TestIDs(t *testing.T) {
	s := newStore(t)
	if ids, err := s.IDs(); err != nil || len(ids) != 0 {
		t.Fatalf("no sb/ yet: %v %v", ids, err)
	}
	for _, id := range []string{"sb-one0001", "sb-two0002", "sb-three03"} {
		if err := s.Create(Meta{ID: id, Image: "i"}); err != nil {
			t.Fatal(err)
		}
	}
	// Wrong-shaped entries under sb/: bad name, a plain file, the server dir.
	os.MkdirAll(filepath.Join(s.Root, "sb", "server"), 0o755)
	os.WriteFile(filepath.Join(s.Root, "sb", "notes.txt"), []byte("x"), 0o644)
	ids, err := s.IDs()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if want := []string{"sb-one0001", "sb-three03", "sb-two0002"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("IDs() = %v, want %v", ids, want)
	}
}

// Create/WriteMeta must report a broken data dir rather than pretend the
// sandbox exists; the reaper treats a write error as "leave it alone".
func TestWriteFailures(t *testing.T) {
	s := newStore(t)
	if err := s.WriteMeta(Meta{ID: "sb-nodir01", Image: "i"}); err == nil {
		t.Error("WriteMeta into a missing dir: want error")
	}
	// sb/<id> taken by a file, so ws/ cannot be made.
	os.MkdirAll(filepath.Join(s.Root, "sb"), 0o755)
	os.WriteFile(filepath.Join(s.Root, "sb", "sb-blocked1"), []byte("x"), 0o644)
	if err := s.Create(Meta{ID: "sb-blocked1", Image: "i"}); err == nil {
		t.Error("Create over a file: want error")
	}
}

func TestOpenRequiresMarker(t *testing.T) {
	root := t.TempDir()
	_, err := Open(root)
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("want a not-mounted error naming the data dir, got %v", err)
	}
	if s, err := Open(filepath.Join(root, "gone")); err == nil || s != nil {
		t.Fatalf("missing data dir accepted: %v", err)
	}
}

// Dir/WS are the contract with the pod's hostPath mount: ws/ is the workspace
// and must never contain meta.json, which the agent could otherwise read.
func TestDataDirLayout(t *testing.T) {
	s := newStore(t)
	if got, want := s.Dir("sb-abc123"), filepath.Join(s.Root, "sb", "sb-abc123"); got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
	if got, want := s.WS("sb-abc123"), filepath.Join(s.Dir("sb-abc123"), "ws"); got != want {
		t.Errorf("WS = %q, want %q", got, want)
	}
	if err := s.Create(Meta{ID: "sb-abc123", Image: "i"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(s.WS("sb-abc123")); err != nil || !fi.IsDir() {
		t.Fatalf("ws/ missing after Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir("sb-abc123"), "meta.json")); err != nil {
		t.Fatalf("meta.json not next to ws/: %v", err)
	}
}

// WriteMeta is a replace-in-place, not an append: the second write must be
// the whole file, and no temp file may survive it.
func TestWriteMetaReplaces(t *testing.T) {
	s := newStore(t)
	if err := s.Create(Meta{ID: "sb-replace", Image: "a"}); err != nil {
		t.Fatal(err)
	}
	big := Meta{ID: "sb-replace", Image: strings.Repeat("b", 4096), Status: "stopped", Reason: "disk_full"}
	if err := s.WriteMeta(big); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadMeta("sb-replace")
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != big.Image || got.Status != "stopped" || got.Reason != "disk_full" || got.V != Version {
		t.Fatalf("rewrite lost data: %+v", got)
	}
	ents, _ := os.ReadDir(s.Dir("sb-replace"))
	for _, e := range ents {
		if e.Name() != "meta.json" && e.Name() != "ws" {
			t.Errorf("WriteMeta left %s behind", e.Name())
		}
	}
	// A bad id never touches the disk.
	if err := s.WriteMeta(Meta{ID: "sb-nope/../x"}); !errors.Is(err, ErrInvalidID) {
		t.Errorf("WriteMeta accepted a bad id: %v", err)
	}
}

func TestReadMeta(t *testing.T) {
	s := newStore(t)
	if _, err := s.ReadMeta("not-an-id"); !errors.Is(err, ErrInvalidID) {
		t.Errorf("bad id: %v", err)
	}
	if _, err := s.ReadMeta("sb-empty01"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing dir: %v", err)
	}
	os.MkdirAll(s.Dir("sb-corrupt"), 0o755)
	os.WriteFile(filepath.Join(s.Dir("sb-corrupt"), "meta.json"), []byte("{not json"), 0o644)
	if _, err := s.ReadMeta("sb-corrupt"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("torn meta must be a hard error, got %v", err)
	}
	os.WriteFile(filepath.Join(s.Dir("sb-corrupt"), "meta.json"), []byte(`{"v":0,"id":"sb-corrupt"}`), 0o644)
	if _, err := s.ReadMeta("sb-corrupt"); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("v=0 is not this build: %v", err)
	}
}

// Every field a caller depends on must survive the JSON round trip, including
// the zero-valued ones that are omitted from the file.
func TestMetaRoundTrip(t *testing.T) {
	s := newStore(t)
	exp := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	grace := exp.Add(10 * time.Minute)
	m := Meta{ID: "sb-fields1", Image: "alpine", Parent: "sb-parent1", Created: exp, ExpiresAt: &exp,
		Status: "running", Reason: "over_disk_limit", Network: "none", CPU: "500m", Memory: "512Mi",
		GraceUntil: &grace, Org: "acme", KeyID: "key-1", Node: "worker-1"}
	if err := s.Create(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadMeta(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.V = 0 // stamped by WriteMeta, not part of the caller's value
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, m)
	}
	// ttl=null (no expiry) must read back as nil, not the zero time.
	forever := Meta{ID: "sb-forever", Image: "i"}
	if err := s.Create(forever); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadMeta(forever.ID); got.ExpiresAt != nil {
		t.Errorf("forever sandbox grew an expiry: %v", got.ExpiresAt)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir(forever.ID), "meta.json"))
	// expires_at is always written (as null) so a reader can tell "no ttl"
	// from a file written before the field existed; the omitempty fields of a
	// bare sandbox must be gone.
	if !strings.Contains(string(raw), `"expires_at": null`) {
		t.Errorf("keep-forever sandbox not written as null expiry: %s", raw)
	}
	for _, absent := range []string{"parent", "status", "reason", "grace_until", "project_id", "org", "key_id", "node"} {
		if strings.Contains(string(raw), `"`+absent+`"`) {
			t.Errorf("empty %s should be omitted, got %s", absent, raw)
		}
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id := NewID()
		if !ValidID(id) {
			t.Fatalf("NewID produced %q, which ValidID rejects", id)
		}
		seen[id] = true
	}
	if len(seen) < 190 {
		t.Errorf("NewID repeats: %d unique of 200", len(seen))
	}
}
