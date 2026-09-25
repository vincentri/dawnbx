package auth

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// open returns a fresh SQLite DB, or a Postgres one when DAWNBX_TEST_DATABASE_URL is set
// (the tables are dropped first, so point it at a scratch database).
func open(t *testing.T) *DB {
	url := os.Getenv("DAWNBX_TEST_DATABASE_URL")
	if url == "" {
		url = filepath.Join(t.TempDir(), "dawnbx.db")
	} else {
		db, err := sql.Open("pgx", url) // raw: Open would migrate before the drop
		if err != nil {
			t.Fatal(err)
		}
		for _, tb := range []string{"audit_log", "sessions", "api_keys", "users", "orgs", "settings", "schema_migrations"} {
			if _, err := db.Exec(`DROP TABLE IF EXISTS ` + tb); err != nil {
				t.Fatal(err)
			}
		}
		db.Close()
	}
	d, err := Open(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestKeys(t *testing.T) {
	d := open(t)
	now := time.Unix(1_800_000_000, 0)
	d.Now = func() time.Time { return now }

	tok, k, err := d.CreateKey(DefaultOrg, "ci", "user:admin", nil)
	if err != nil || !strings.HasPrefix(tok, "dbx_"+k.ID+"_") {
		t.Fatal(tok, err)
	}
	p, err := d.CheckKey(tok)
	if err != nil || p.KeyID != k.ID || p.Org != DefaultOrg || p.Admin {
		t.Fatal(p, err)
	}
	if _, err := d.CheckKey(tok + "x"); !errors.Is(err, ErrUnauthorized) {
		t.Error("wrong key accepted", err)
	}
	if _, err := d.CheckKey(""); !errors.Is(err, ErrUnauthorized) {
		t.Error("empty key accepted")
	}
	keys, _ := d.ListKeys(DefaultOrg)
	if len(keys) != 1 || keys[0].LastUsed == nil || keys[0].Name != "ci" {
		t.Fatalf("%+v", keys)
	}
	if ok, _ := d.RevokeKey("other-org", k.ID); ok {
		t.Error("revoked across orgs")
	}
	if ok, err := d.RevokeKey(DefaultOrg, k.ID); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := d.CheckKey(tok); !errors.Is(err, ErrUnauthorized) {
		t.Error("revoked key still works on this node")
	}

	exp := now.Add(time.Hour)
	tok2, _, _ := d.CreateKey(DefaultOrg, "short", "user:admin", &exp)
	if _, err := d.CheckKey(tok2); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour) // also past the 30 s cache
	if _, err := d.CheckKey(tok2); !errors.Is(err, ErrUnauthorized) {
		t.Error("expired key accepted")
	}
}

func TestImportKeyFile(t *testing.T) {
	d := open(t)
	path := filepath.Join(t.TempDir(), "api-keys.json")
	write := func(keys ...string) {
		var parts []string
		for _, k := range keys {
			s := sha256.Sum256([]byte(k))
			parts = append(parts, `{"sha256":"`+hex.EncodeToString(s[:])+`","created":"2026-09-01T00:00:00Z"}`)
		}
		os.WriteFile(path, []byte(`{"v":1,"keys":[`+strings.Join(parts, ",")+`]}`), 0o600)
		if err := d.ImportKeyFile(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.ImportKeyFile(path); err != nil {
		t.Fatal("missing file should be fine:", err)
	}
	write("dawnbx_old")
	write("dawnbx_old") // restart: no duplicate
	if p, err := d.CheckKey("dawnbx_old"); err != nil || p.Org != DefaultOrg {
		t.Fatal(p, err)
	}
	write("dawnbx_new") // install.sh --new-key
	if _, err := d.CheckKey("dawnbx_old"); !errors.Is(err, ErrUnauthorized) {
		t.Error("old installer key survived --new-key")
	}
	if _, err := d.CheckKey("dawnbx_new"); err != nil {
		t.Error(err)
	}
	keys, _ := d.ListKeys("")
	if len(keys) != 2 {
		t.Errorf("%+v", keys)
	}
}

func TestLogin(t *testing.T) {
	d := open(t)
	if err := d.EnsureAdmin("admin", "pw1"); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureAdmin("admin", "pw1"); err != nil { // restart, same env
		t.Fatal(err)
	}
	if _, _, err := d.Login("admin", "nope"); !errors.Is(err, ErrUnauthorized) {
		t.Error("wrong password", err)
	}
	if _, _, err := d.Login("nobody", "pw1"); !errors.Is(err, ErrUnauthorized) {
		t.Error("unknown user", err)
	}
	tok, p, err := d.Login("admin", "pw1")
	if err != nil || !p.Admin || p.User != "admin" {
		t.Fatal(p, err)
	}
	if p, err := d.CheckSession(tok); err != nil || !p.Admin || p.Actor() != "user:admin" {
		t.Fatal(p, err)
	}
	if _, err := d.CheckKey(tok); err == nil {
		t.Error("session token accepted as API key")
	}

	// A new env password ends old sessions.
	if err := d.EnsureAdmin("admin", "pw2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CheckSession(tok); !errors.Is(err, ErrUnauthorized) {
		t.Error("session survived password reset")
	}
	tok, _, err = d.Login("admin", "pw2")
	if err != nil {
		t.Fatal(err)
	}
	d.Logout(tok)
	if _, err := d.CheckSession(tok); !errors.Is(err, ErrUnauthorized) {
		t.Error("session survived logout")
	}

	tok, _, _ = d.Login("admin", "pw2")
	d.Now = func() time.Time { return time.Now().Add(SessionTTL + time.Minute) }
	if _, err := d.CheckSession(tok); !errors.Is(err, ErrUnauthorized) {
		t.Error("expired session accepted")
	}
}

func TestAuditAndReopen(t *testing.T) {
	d := open(t)
	d.Audit(DefaultOrg, "user:admin", "key.create", "abc")
	d.Audit("acme", "key:x", "sandbox.create", "sb-1")
	if ev, err := d.Events(DefaultOrg, 10); err != nil || len(ev) != 1 || ev[0].Action != "key.create" {
		t.Fatal(ev, err)
	}
	if ev, _ := d.Events("", 10); len(ev) != 2 {
		t.Fatal(ev)
	}
	// Migrations are idempotent across restarts.
	if err := d.migrate(); err != nil {
		t.Fatal(err)
	}
}

func TestUsersOrgs(t *testing.T) {
	d := open(t)
	if err := d.CreateOrg("acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateOrg("acme", "Acme"); !errors.Is(err, ErrExists) {
		t.Error("dup org", err)
	}
	if l, err := d.ListOrgs(); err != nil || len(l) != 2 {
		t.Error("orgs", l, err)
	}
	if _, err := d.CreateUser("acme", "bob", "bob-password", "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("acme", "bob", "bob-password", "member"); !errors.Is(err, ErrExists) {
		t.Error("dup user", err)
	}
	tok, p, err := d.Login("bob", "bob-password")
	if err != nil || p.Admin || p.Org != "acme" {
		t.Fatal(p, err)
	}
	if err := d.SetPassword("bob", "bob-password-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CheckSession(tok); !errors.Is(err, ErrUnauthorized) {
		t.Error("session survived SetPassword")
	}
	if err := d.CheckPassword("bob", "bob-password-2"); err != nil {
		t.Error(err)
	}
	if l, err := d.ListUsers("acme"); err != nil || len(l) != 1 || l[0].Username != "bob" {
		t.Error("users", l, err)
	}
	if ok, err := d.DeleteUser("bob"); !ok || err != nil {
		t.Error("delete", ok, err)
	}
	if _, _, err := d.Login("bob", "bob-password-2"); !errors.Is(err, ErrUnauthorized) {
		t.Error("deleted user logged in", err)
	}
}
