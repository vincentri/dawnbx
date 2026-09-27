package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// openAt is open for a caller-chosen file, so a test can look at the bytes on
// disk or open a second handle to the same database (another API node).
func openAt(t *testing.T, path string) *DB {
	t.Helper()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestSecretsAreOnlyHashed: the token is shown once and never stored, so a
// stolen database file cannot be replayed as an API key or a session.
func TestSecretsAreOnlyHashed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dawnbx.db")
	d := openAt(t, path)
	tok, k, err := d.CreateKey(DefaultOrg, "ci", "user:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	// dbx_<id>_<48 hex chars of secret>: the id names the row, the secret is random.
	parts := strings.Split(tok, "_")
	if len(parts) != 3 || parts[0] != "dbx" || parts[1] != k.ID || len(parts[1]) != 12 {
		t.Fatalf("token %q does not look like dbx_<id>_<secret>", tok)
	}
	if _, err := hex.DecodeString(parts[2]); err != nil || len(parts[2]) != 48 {
		t.Errorf("secret %q is not 24 random bytes in hex", parts[2])
	}

	var h, by, name string
	if err := d.db.QueryRow(`SELECT hash, created_by, name FROM api_keys WHERE id = $1`, k.ID).Scan(&h, &by, &name); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok))
	if h != hex.EncodeToString(sum[:]) {
		t.Errorf("stored hash %q is not sha256 of the token", h)
	}
	if by != "user:admin" || name != "ci" {
		t.Errorf("key row: by=%q name=%q", by, name)
	}

	// Sessions get the same treatment: the cookie value is the secret.
	if err := d.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	stok, _, err := d.Login("admin", "admin-password")
	if err != nil {
		t.Fatal(err)
	}
	var sh string
	if err := d.db.QueryRow(`SELECT hash FROM sessions WHERE user_id = (SELECT id FROM users WHERE username='admin')`).Scan(&sh); err != nil {
		t.Fatal(err)
	}
	ssum := sha256.Sum256([]byte(stok))
	if sh != hex.EncodeToString(ssum[:]) {
		t.Errorf("session hash %q is not sha256 of the token", sh)
	}

	// And neither plaintext is anywhere in the database files, WAL included.
	var seen []string
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue // the WAL may not exist yet
		}
		seen = append(seen, string(b))
		if strings.Contains(string(b), tok) {
			t.Errorf("%s holds the plaintext API key", f)
		}
		if strings.Contains(string(b), stok) {
			t.Errorf("%s holds the plaintext session token", f)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no database file to inspect")
	}
	// Sanity: the row really is in one of them, so the checks above read
	// something and not an empty file.
	if !strings.Contains(strings.Join(seen, ""), h) {
		t.Fatal("no api_keys row on disk; the plaintext checks prove nothing")
	}
}

// TestKeyCacheWindow: a key revoked on another API node is honoured for at most
// cacheTTL, then that node re-reads the row and refuses it.
func TestKeyCacheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dawnbx.db")
	a := openAt(t, path) // the node under test
	b := openAt(t, path) // a second API node sharing the database
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }

	tok, k, err := a.CreateKey(DefaultOrg, "ci", "user:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := a.CheckKey(tok); err != nil || p.KeyID != k.ID {
		t.Fatal(p, err)
	}
	if ok, err := b.RevokeKey(DefaultOrg, k.ID); !ok || err != nil {
		t.Fatal(ok, err)
	}

	// Still inside the window: served from the cache, revocation not yet seen.
	now = now.Add(cacheTTL - time.Second)
	if p, err := a.CheckKey(tok); err != nil || p.KeyID != k.ID {
		t.Errorf("inside the 30s window: %v %v", p, err)
	}
	// One second later the entry is stale and the revoked row wins.
	now = now.Add(2 * time.Second)
	if _, err := a.CheckKey(tok); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("after the window a revoked key still works: %v", err)
	}
	// The node that did the revoking forgets its own copy at once.
	if _, err := b.CheckKey(tok); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("revoking node kept serving the key: %v", err)
	}
}

// TestCacheIsBounded: the token cache drops everything past 10k entries rather
// than growing with every key a busy server sees.
func TestCacheIsBounded(t *testing.T) {
	d := open(t)
	tok, _, err := d.CreateKey(DefaultOrg, "ci", "user:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10001; i++ {
		d.cache["k:junk-"+strconv.Itoa(i)] = cached{}
	}
	if len(d.cache) <= 10000 {
		t.Fatalf("setup: cache has %d entries", len(d.cache))
	}
	if _, err := d.CheckKey(tok); err != nil {
		t.Fatal(err)
	}
	if len(d.cache) != 1 {
		t.Errorf("cache holds %d entries after a lookup, want just the one", len(d.cache))
	}
}

// TestSessionLifecycle: an empty token is refused, and the row that survives is
// the hash, not the cookie value.
func TestSessionLifecycle(t *testing.T) {
	d := open(t)
	if _, err := d.CheckSession(""); !errors.Is(err, ErrUnauthorized) {
		t.Error("empty session token accepted", err)
	}
	if err := d.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	tok, p, err := d.Login("admin", "admin-password")
	if err != nil || !p.Admin {
		t.Fatal(p, err)
	}
	if got, err := d.CheckSession(tok); err != nil || got.Actor() != "user:admin" {
		t.Fatal(got, err)
	}
	// The second check is the cached one; it must agree with the first.
	if got, err := d.CheckSession(tok); err != nil || got.Org != DefaultOrg {
		t.Fatal(got, err)
	}
	// Logging out drops the cache entry as well as the row.
	if err := d.Logout(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CheckSession(tok); !errors.Is(err, ErrUnauthorized) {
		t.Error("session came back after logout", err)
	}
}

// TestKeyAndUserConstraints: rows that would cross an org boundary or a foreign
// key are refused instead of being half-written.
func TestKeyAndUserConstraints(t *testing.T) {
	d := open(t)
	if tok, k, err := d.CreateKey("ghost", "ci", "user:admin", nil); err == nil {
		t.Errorf("key made in a nonexistent org: %q %+v", tok, k)
	}
	if u, err := d.CreateUser("ghost", "bob", "bob-password", "member"); err == nil {
		t.Errorf("user made in a nonexistent org: %+v", u)
	}
	if err := d.CreateOrg("acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("acme", "bob", "bob-password", "member"); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.OrgExists("acme"); err != nil || !ok {
		t.Errorf("OrgExists(acme) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := d.OrgExists("ghost"); err != nil || ok {
		t.Errorf("OrgExists(ghost) = %v, %v; want false, nil", ok, err)
	}
	// One row per username across every org, and SetPassword says so when the
	// user is not there at all.
	if _, err := d.CreateUser(DefaultOrg, "bob", "bob-password", "member"); !errors.Is(err, ErrExists) {
		t.Error("same username in two orgs", err)
	}
	if err := d.SetPassword("nobody", "x-password"); err == nil {
		t.Error("SetPassword invented a user")
	}
	if ok, err := d.DeleteUser("nobody"); ok || err != nil {
		t.Errorf("DeleteUser of a missing user: %v %v", ok, err)
	}
	l, err := d.ListUsers("")
	if err != nil || len(l) != 1 || l[0].Org != "acme" {
		t.Errorf("ListUsers across orgs: %+v %v", l, err)
	}
	if err := d.CheckPassword("bob", "bob-password"); err != nil {
		t.Error(err)
	}
}

// TestEnsureAdminEnvWins: install.sh's admin.env is the root of trust. A new
// value takes over the account, and an unchanged value does not undo a password
// changed in the dashboard.
func TestEnsureAdminEnvWins(t *testing.T) {
	d := open(t)
	if err := d.EnsureAdmin("ops", "ops-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser(DefaultOrg, "root", "root-password", "member"); err != nil {
		t.Fatal(err)
	}
	// A member that install.sh names as the admin is promoted, and the env
	// password replaces whatever the dashboard set.
	if err := d.SetPassword("root", "dashboard-password"); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureAdmin("root", "env-password"); err != nil {
		t.Fatal(err)
	}
	_, p, err := d.Login("root", "env-password")
	if err != nil || !p.Admin {
		t.Fatalf("env admin: %+v %v", p, err)
	}
	if err := d.CheckPassword("root", "dashboard-password"); err == nil {
		t.Error("the env password did not replace the dashboard one")
	}
	// A dashboard change now survives the restart, because the env value is unchanged.
	if err := d.SetPassword("root", "dashboard-password-2"); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureAdmin("root", "env-password"); err != nil {
		t.Fatal(err)
	}
	if err := d.CheckPassword("root", "dashboard-password-2"); err != nil {
		t.Error("restart reverted the dashboard password", err)
	}
	if err := d.EnsureAdmin("ops", "ops-password"); err != nil {
		t.Fatal(err)
	}
}

// TestDBFailuresAreNotSuccess: with the database gone every call has to say so.
// Reporting "revoked" or "deleted" when nothing was written would be worse than
// an error.
func TestDBFailuresAreNotSuccess(t *testing.T) {
	d := open(t)
	if err := d.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	tok, k, err := d.CreateKey(DefaultOrg, "ci", "user:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Login("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	d.Close()

	if ok, err := d.RevokeKey(DefaultOrg, k.ID); ok || err == nil {
		t.Errorf("RevokeKey reported %v/%v with no database", ok, err)
	}
	if ok, err := d.DeleteUser("admin"); ok || err == nil {
		t.Errorf("DeleteUser reported %v/%v with no database", ok, err)
	}
	for name, err := range map[string]error{
		"CheckKey":      checkErr(d.CheckKey(tok)),
		"CheckSession":  checkErr(d.CheckSession(tok)),
		"CheckPassword": d.CheckPassword("admin", "admin-password"),
		"ListKeys":      first(d.ListKeys(DefaultOrg)),
		"ListUsers":     first(d.ListUsers("")),
		"ListOrgs":      first(d.ListOrgs()),
		"Events":        first(d.Events("", 10)),
		"CreateKey":     createErr(d),
		"CreateUser":    createUserErr(d),
		"Login":         loginErr(d),
		"EnsureAdmin":   d.EnsureAdmin("admin", "admin-password"),
		"SetPassword":   d.SetPassword("admin", "another-password"),
		"CreateOrg":     d.CreateOrg("acme", "Acme"),
	} {
		if err == nil {
			t.Errorf("%s claimed success with no database", name)
		}
	}
	// An unauthorized answer would send the caller off to rotate a key that is
	// still fine; a dead database must not look like that.
	if err := checkErr(d.CheckKey(tok)); errors.Is(err, ErrUnauthorized) {
		t.Error("a dead database was reported as an unauthorized key")
	}
}

func checkErr(_ *Principal, err error) error { return err }
func createErr(d *DB) error                  { _, _, err := d.CreateKey(DefaultOrg, "x", "b", nil); return err }
func createUserErr(d *DB) error {
	_, err := d.CreateUser(DefaultOrg, "x", "x-password", "member")
	return err
}
func loginErr(d *DB) error { _, _, err := d.Login("admin", "admin-password"); return err }

func first[T any](_ []T, err error) error { return err }

// TestMigrateRefusesToDowngrade: a database from a newer build is not opened,
// and a migration that fails is rolled back and named.
func TestMigrateRefusesToDowngrade(t *testing.T) {
	d := open(t)
	if _, err := d.db.Exec(`INSERT INTO schema_migrations (version) VALUES ($1)`, len(migrations)+7); err != nil {
		t.Fatal(err)
	}
	err := d.migrate()
	if err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("newer schema accepted: %v", err)
	}
	if _, err := d.db.Exec(`DELETE FROM schema_migrations WHERE version > $1`, len(migrations)); err != nil {
		t.Fatal(err)
	}

	// Make migration 2 fail: something else already owns the settings table.
	if _, err := d.db.Exec(`DROP TABLE settings`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`CREATE TABLE settings (k TEXT PRIMARY KEY, v TEXT, other TEXT)`); err != nil {
		t.Fatal(err)
	}
	// Everything from the settings migration on is unapplied, so migrate()
	// starts there and hits the failure again.
	if _, err := d.db.Exec(`DELETE FROM schema_migrations WHERE version >= 2`); err != nil {
		t.Fatal(err)
	}
	err = d.migrate()
	if err == nil || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("failing migration not reported: %v", err)
	}
	// The version row was not advanced, so the next start retries.
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 2`).Scan(&n); err != nil || n != 0 {
		t.Errorf("failed migration left a version row: n=%d %v", n, err)
	}
}

// TestRedactKeepsPasswordsOutOfLogs: the error a bad -database-url produces may
// be read in a support ticket, so the password must not be in it.
func TestRedactKeepsPasswordsOutOfLogs(t *testing.T) {
	if got := redact("postgres://user:hunter2@db.internal:5432/dawnbx"); got != "postgres://…@db.internal:5432/dawnbx" {
		t.Errorf("redact = %q", got)
	}
	if got := redact("/var/lib/dawnbx/server/dawnbx.db"); got != "/var/lib/dawnbx/server/dawnbx.db" {
		t.Errorf("redact = %q", got)
	}
	if got := redact("someone@dawnbx"); got != "someone@dawnbx" {
		t.Errorf("redact = %q", got)
	}
	_, err := Open("postgres://user:hunter2@127.0.0.1:1/dawnbx")
	if err == nil {
		t.Fatal("opened a database that is not there")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("Open error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "postgres://…@127.0.0.1:1/dawnbx") {
		t.Errorf("Open error should name the redacted url: %v", err)
	}
}

// MintPassword is the one generator behind every administrator password the
// product hands out, so its two properties are worth pinning: the length asked
// for is the length returned, and nothing outside the alphabet ever appears.
func TestMintPasswordLengthAndAlphabet(t *testing.T) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	seen := map[rune]bool{}
	for _, n := range []int{20, 24} {
		pw, err := MintPassword(n)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != n {
			t.Errorf("MintPassword(%d) returned %d characters", n, len(pw))
		}
		for _, c := range pw {
			if !strings.ContainsRune(alphabet, c) {
				t.Errorf("character %q is outside the alphabet an installer and a copy-paste handle", c)
			}
			seen[c] = true
		}
	}
	// Two mints are not the same password.
	a, _ := MintPassword(24)
	b, _ := MintPassword(24)
	if a == b {
		t.Error("two mints returned the same password")
	}
}

// Every credential in this file is built from randRead. When the entropy source
// fails, crypto/rand leaves the buffer zeroed and the caller used to carry on,
// which hands out a token an attacker can predict: the key becomes a constant,
// and so does the session. Each of these must refuse instead.
func TestAFailingEntropySourceRefusesRatherThanMints(t *testing.T) {
	d := open(t)
	if err := d.CreateOrg("acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("acme", "bob", "bob-password", "member"); err != nil {
		t.Fatal(err)
	}

	real := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("entropy source unavailable") }
	t.Cleanup(func() { randRead = real })

	if _, err := randID(12); err == nil {
		t.Error("randID returned an id from a zero-filled buffer")
	}
	if pw, err := MintPassword(24); err == nil {
		t.Errorf("MintPassword returned %q from a zero-filled buffer", pw)
	}
	if tok, k, err := d.CreateKey("acme", "ci", "bob", nil); err == nil {
		t.Errorf("CreateKey returned %q (%v) from a zero-filled buffer", tok, k.ID)
	}
	if tok, p, err := d.Login("bob", "bob-password"); err == nil {
		t.Errorf("Login issued session %q for %v from a zero-filled buffer", tok, p)
	}
	if err := d.Audit("acme", "user:bob", "test", "x"); err == nil {
		t.Error("Audit wrote a row with a predictable id")
	}
	if err := d.RecordOp("acme", "create", "validating", ""); err == nil {
		t.Error("RecordOp wrote a row with a predictable id")
	}

	// And with entropy restored, the same calls work, so the failures above are
	// the seam and not a broken database.
	randRead = real
	if _, err := randID(12); err != nil {
		t.Errorf("randID failed with real entropy: %v", err)
	}
	if _, _, err := d.CreateKey("acme", "ci", "bob", nil); err != nil {
		t.Errorf("CreateKey failed with real entropy: %v", err)
	}
}
