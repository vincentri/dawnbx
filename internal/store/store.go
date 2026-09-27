// Package store owns the on-disk layout of the data dir:
//
//	<data>/.dawnbx-volume          marker written by install.sh (R21)
//	<data>/server/              server identity, never mounted into sandboxes (R18)
//	<data>/sb/<id>/meta.json    source of truth for sandbox metadata (R16)
//	<data>/sb/<id>/ws/          mounted at /workspace (R15)
package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Version is the meta.json schema version this build understands.
const Version = 1

const Marker = ".dawnbx-volume"

var (
	ErrNotFound       = errors.New("sandbox not found")
	ErrUnknownVersion = errors.New("meta.json version not supported by this build")
	ErrInvalidID      = errors.New("invalid sandbox id")
)

// IDs double as pod names, so they must be DNS-1123 labels.
var idRe = regexp.MustCompile(`^sb-[a-z0-9]{6,20}$`)

func ValidID(id string) bool { return idRe.MatchString(id) }

func NewID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "sb-" + string(b)
}

type Meta struct {
	V         int        `json:"v"`
	ID        string     `json:"id"`
	Image     string     `json:"image"`
	Parent    string     `json:"parent,omitempty"`
	Created   time.Time  `json:"created"`
	ExpiresAt *time.Time `json:"expires_at"`       // nil = keep forever (ttl=None)
	Status    string     `json:"status,omitempty"` // running | stopped | deleting
	Reason    string     `json:"reason,omitempty"` // why stopped: disk_full, over_disk_limit
	Network   string     `json:"network,omitempty"`
	CPU       string     `json:"cpu,omitempty"`
	Memory    string     `json:"memory,omitempty"`
	// GraceUntil skips the per-sandbox disk cap after start() so the user can clean up.
	GraceUntil *time.Time `json:"grace_until,omitempty"`
	Org        string     `json:"org,omitempty"`    // owning org; empty = "default" (made before orgs)
	KeyID      string     `json:"key_id,omitempty"` // API key that created it; empty for dashboard users
	// Node holds /workspace (hostPath on its disk); the pod is pinned there. Empty = not scheduled yet.
	Node string `json:"node,omitempty"`
}

type Store struct{ Root string }

// Open refuses a data dir without the install marker: an unmounted volume
// looks like an empty dir, and treating that as "all sandboxes gone" loses data.
func Open(root string) (*Store, error) {
	if _, err := os.Stat(filepath.Join(root, Marker)); err != nil {
		return nil, fmt.Errorf("data dir %s not mounted (marker missing). Mount the volume, then systemctl restart dawnbx", root)
	}
	return &Store{Root: root}, nil
}

func (s *Store) Dir(id string) string { return filepath.Join(s.Root, "sb", id) }
func (s *Store) WS(id string) string  { return filepath.Join(s.Dir(id), "ws") }

// IDs lists every entry under sb/, valid meta or not.
func (s *Store) IDs() ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(s.Root, "sb"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		// A directory that cannot be read is not an empty one. Returning the
		// partial list with a nil error is what let four call sites answer "no
		// sandboxes" when the truth was that the store could not be read.
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() && ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// Create makes <id>/ws and writes the first meta.json.
func (s *Store) Create(m Meta) error {
	if !ValidID(m.ID) {
		return ErrInvalidID
	}
	if err := os.MkdirAll(s.WS(m.ID), 0o755); err != nil {
		return err
	}
	return s.WriteMeta(m)
}

// WriteMeta replaces meta.json atomically: a crash leaves either the old or
// the new file, never a torn one.
func (s *Store) WriteMeta(m Meta) error {
	if !ValidID(m.ID) {
		return ErrInvalidID
	}
	m.V = Version
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := s.Dir(m.ID)
	f, err := os.CreateTemp(dir, "meta.json.tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "meta.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ReadMeta returns ErrUnknownVersion for files written by a newer build so
// callers leave those sandboxes untouched instead of treating them as orphans.
func (s *Store) ReadMeta(id string) (Meta, error) {
	var m Meta
	if !ValidID(id) {
		return m, ErrInvalidID
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(id), "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", id, err)
	}
	if m.V != Version {
		return m, fmt.Errorf("%s: v=%d: %w", id, m.V, ErrUnknownVersion)
	}
	return m, nil
}
