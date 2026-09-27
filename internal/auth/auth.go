// Package auth stores orgs, users, API keys, sessions and the audit log in
// SQLite (default, one file on the data volume) or Postgres (several API nodes).
// Queries are plain SQL that both accept; $N placeholders work in SQLite too.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
	"modernc.org/sqlite"
)

// DefaultOrg owns everything made before orgs existed.
const DefaultOrg = "default"

const (
	SessionTTL = 7 * 24 * time.Hour
	cacheTTL   = 30 * time.Second // a key revoked on another API node works this long there
)

var ErrUnauthorized = errors.New("unauthorized")

// Each entry runs once, in order, inside a transaction.
var migrations = []string{`
CREATE TABLE orgs (id TEXT PRIMARY KEY, name TEXT NOT NULL, created BIGINT NOT NULL);
CREATE TABLE users (id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id), username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL, role TEXT NOT NULL, created BIGINT NOT NULL);
CREATE TABLE api_keys (id TEXT PRIMARY KEY, org_id TEXT NOT NULL REFERENCES orgs(id), name TEXT NOT NULL,
  hash TEXT NOT NULL UNIQUE, created BIGINT NOT NULL, created_by TEXT NOT NULL,
  last_used BIGINT, expires BIGINT, revoked BIGINT);
CREATE TABLE sessions (hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created BIGINT NOT NULL, expires BIGINT NOT NULL);
CREATE TABLE audit_log (id TEXT PRIMARY KEY, at BIGINT NOT NULL, org_id TEXT NOT NULL, actor TEXT NOT NULL,
  action TEXT NOT NULL, target TEXT NOT NULL);
CREATE INDEX audit_log_org_at ON audit_log (org_id, at);
INSERT INTO orgs (id, name, created) VALUES ('default', 'default', 0)`, `
CREATE TABLE settings (k TEXT PRIMARY KEY, v TEXT NOT NULL)`, `
-- The cluster tables. Two spellings differ from the data model, because one
-- SQL string has to work on both engines: bytea rather than blob (blob is not
-- a Postgres type, and SQLite takes any type name), and double precision for
-- the prices (Postgres real is float32, which would round 0.0168 to
-- 0.016800001 while SQLite's real is float64). The children cascade, so
-- deleting a cluster row forgets its secrets, workers and phase history at once.
CREATE TABLE IF NOT EXISTS clusters (name TEXT PRIMARY KEY, provider TEXT NOT NULL, region TEXT NOT NULL,
  instance_type TEXT NOT NULL, disk_gib INTEGER NOT NULL, domain TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL, phase TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '',
  quote_id TEXT NOT NULL, hourly_usd DOUBLE PRECISION NOT NULL, monthly_usd DOUBLE PRECISION NOT NULL,
  provider_state TEXT NOT NULL DEFAULT '{}', url TEXT NOT NULL DEFAULT '',
  tls_pin TEXT NOT NULL DEFAULT '', created BIGINT NOT NULL, updated BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS cluster_credentials (cluster TEXT PRIMARY KEY REFERENCES clusters(name) ON DELETE CASCADE,
  admin_password BYTEA NOT NULL, api_key BYTEA, created BIGINT NOT NULL, rotated BIGINT);
CREATE TABLE IF NOT EXISTS cluster_nodes (cluster TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
  id TEXT NOT NULL, instance_type TEXT NOT NULL, status TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '',
  sandboxes INTEGER NOT NULL DEFAULT 0, created BIGINT NOT NULL, PRIMARY KEY (cluster, id));
CREATE TABLE IF NOT EXISTS cluster_ops (id TEXT PRIMARY KEY,
  cluster TEXT NOT NULL REFERENCES clusters(name) ON DELETE CASCADE,
  kind TEXT NOT NULL, phase TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '', created BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS cluster_ops_cluster_kind ON cluster_ops (cluster, kind, created)`,
}

// Principal is who a request acts as.
type Principal struct {
	Org   string `json:"org"`
	User  string `json:"user,omitempty"`   // set for dashboard sessions
	KeyID string `json:"key_id,omitempty"` // set for API keys
	Admin bool   `json:"admin"`            // sees every org's sandboxes, manages keys
}

func (p *Principal) Actor() string {
	if p.User != "" {
		return "user:" + p.User
	}
	return "key:" + p.KeyID
}

type cached struct {
	p   *Principal
	exp time.Time
}

type DB struct {
	db  *sql.DB
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cached // "k:" or "s:" + sha256 hex of the token
}

// Open takes a postgres:// URL or a SQLite file path and migrates it.
func Open(url string) (*DB, error) {
	var db *sql.DB
	var err error
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		db, err = sql.Open("pgx", url)
	} else {
		// One writer at a time; busy_timeout waits instead of failing under concurrent writes.
		db, err = sql.Open("sqlite", "file:"+url+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
		db.SetMaxOpenConns(1)
	}
	if err != nil {
		return nil, err
	}
	d := &DB{db: db, Now: time.Now, cache: map[string]cached{}}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database %s: %w", redact(url), err)
	}
	return d, nil
}

func (d *DB) Close() error { return d.db.Close() }

func redact(url string) string {
	if i := strings.Index(url, "@"); i > 0 && strings.Contains(url, "://") {
		return url[:strings.Index(url, "://")+3] + "…" + url[i:]
	}
	return url
}

// ponytail: two API nodes migrating at once make one fail on the duplicate
// version row; systemd restarts it and it finds the work done.
func (d *DB) migrate() error {
	if _, err := d.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	var have int
	if err := d.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&have); err != nil {
		return err
	}
	if have > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this build (%d); upgrade dawnbx-server", have, len(migrations))
	}
	for v := have + 1; v <= len(migrations); v++ {
		tx, err := d.db.Begin()
		if err != nil {
			return err
		}
		for _, stmt := range strings.Split(migrations[v-1], ";\n") {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", v, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES ($1)`, v); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func randID(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func hash(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

func unix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func (d *DB) lookup(h string) *Principal {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.cache[h]; ok && d.Now().Before(c.exp) {
		return c.p
	}
	return nil
}

func (d *DB) remember(h string, p *Principal) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.cache) > 10000 { // ponytail: drop everything past 10k tokens; an LRU if that ever churns
		clear(d.cache)
	}
	d.cache[h] = cached{p, d.Now().Add(cacheTTL)}
}

func (d *DB) forget(h string) {
	d.mu.Lock()
	delete(d.cache, h)
	d.mu.Unlock()
}

// ponytail: orgs can't be renamed or deleted; add it when a tenant leaves.
func (d *DB) CreateOrg(id, name string) error {
	_, err := d.db.Exec(`INSERT INTO orgs (id, name, created) VALUES ($1, $2, $3)`, id, name, d.Now().Unix())
	if isDuplicate(err) {
		return ErrExists
	}
	return err
}

// --- API keys ---

// Key is an API key as listed; the secret itself is never stored.
type Key struct {
	ID        string     `json:"id"`
	Org       string     `json:"org"`
	Name      string     `json:"name"`
	Created   time.Time  `json:"created"`
	CreatedBy string     `json:"created_by"`
	LastUsed  *time.Time `json:"last_used"`
	Expires   *time.Time `json:"expires"`
	Revoked   *time.Time `json:"revoked"`
}

// CreateKey returns the plaintext token once: dbx_<id>_<secret>.
func (d *DB) CreateKey(org, name, by string, expires *time.Time) (string, *Key, error) {
	k := &Key{ID: randID(12), Org: org, Name: name, Created: d.Now().UTC().Truncate(time.Second), CreatedBy: by, Expires: expires}
	secret := make([]byte, 24)
	rand.Read(secret)
	tok := "dbx_" + k.ID + "_" + hex.EncodeToString(secret)
	_, err := d.db.Exec(`INSERT INTO api_keys (id, org_id, name, hash, created, created_by, expires) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		k.ID, org, name, hash(tok), k.Created.Unix(), by, unix(expires))
	if err != nil {
		return "", nil, err
	}
	return tok, k, nil
}

func (d *DB) ListKeys(org string) ([]Key, error) {
	q := `SELECT id, org_id, name, created, created_by, last_used, expires, revoked FROM api_keys`
	args := []any{}
	if org != "" {
		q += ` WHERE org_id = $1`
		args = append(args, org)
	}
	rows, err := d.db.Query(q+` ORDER BY created, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var k Key
		var created int64
		var used, exp, rev sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Org, &k.Name, &created, &k.CreatedBy, &used, &exp, &rev); err != nil {
			return nil, err
		}
		k.Created = time.Unix(created, 0).UTC()
		k.LastUsed, k.Expires, k.Revoked = ptime(used), ptime(exp), ptime(rev)
		out = append(out, k)
	}
	return out, rows.Err()
}

func ptime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}

// RevokeKey reports false if org has no such live key. Empty org = any org.
func (d *DB) RevokeKey(org, id string) (bool, error) {
	var h string
	err := d.db.QueryRow(`SELECT hash FROM api_keys WHERE id = $1 AND ($2 = '' OR org_id = $2) AND revoked IS NULL`, id, org).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := d.db.Exec(`UPDATE api_keys SET revoked = $1 WHERE id = $2`, d.Now().Unix(), id); err != nil {
		return false, err
	}
	d.forget("k:" + h)
	return true, nil
}

// CheckKey resolves a bearer token. Valid keys are cached for 30 s.
func (d *DB) CheckKey(tok string) (*Principal, error) {
	if tok == "" {
		return nil, ErrUnauthorized
	}
	h := "k:" + hash(tok)
	if p := d.lookup(h); p != nil {
		return p, nil
	}
	var p Principal
	var exp, rev sql.NullInt64
	err := d.db.QueryRow(`SELECT id, org_id, expires, revoked FROM api_keys WHERE hash = $1`, h[2:]).Scan(&p.KeyID, &p.Org, &exp, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	now := d.Now()
	if rev.Valid || (exp.Valid && now.Unix() >= exp.Int64) {
		return nil, ErrUnauthorized
	}
	// Written at most once per cache period per node, so hot keys don't write on every call.
	d.db.Exec(`UPDATE api_keys SET last_used = $1 WHERE id = $2`, now.Unix(), p.KeyID)
	d.remember(h, &p)
	return &p, nil
}

// ImportKeyFile syncs install.sh's api-keys.json (sha256 hashes, no ids) into
// the default org: new hashes are added, installer keys no longer listed are
// revoked, so `install.sh --new-key` still retires the old key.
func (d *DB) ImportKeyFile(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kf struct {
		Keys []struct {
			SHA256  string    `json:"sha256"`
			Created time.Time `json:"created"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(b, &kf); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	keep := map[string]bool{}
	for _, k := range kf.Keys {
		if len(k.SHA256) != 64 {
			continue
		}
		keep[k.SHA256] = true
		created := k.Created
		if created.IsZero() {
			created = d.Now()
		}
		// ON CONFLICT DO NOTHING is the same in SQLite and Postgres.
		if _, err := d.db.Exec(`INSERT INTO api_keys (id, org_id, name, hash, created, created_by) VALUES ($1, $2, 'installer', $3, $4, 'install.sh')
			ON CONFLICT (hash) DO NOTHING`, randID(12), DefaultOrg, k.SHA256, created.Unix()); err != nil {
			return err
		}
	}
	rows, err := d.db.Query(`SELECT id, hash FROM api_keys WHERE created_by = 'install.sh' AND revoked IS NULL`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return err
		}
		if !keep[h] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if _, err := d.RevokeKey("", id); err != nil {
			return err
		}
	}
	return nil
}

// --- users and sessions ---

// EnsureAdmin creates the admin user from the env password. Later it only
// resets the password when the env value changes, so a password changed in the
// dashboard survives restarts, and editing the env still recovers a lost login.
func (d *DB) EnsureAdmin(username, password string) error {
	var id string
	err := d.db.QueryRow(`SELECT id FROM users WHERE username = $1`, username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := d.CreateUser(DefaultOrg, username, password, "admin"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		var applied string
		switch err := d.db.QueryRow(`SELECT v FROM settings WHERE k = 'admin_env'`).Scan(&applied); {
		case err == nil, errors.Is(err, sql.ErrNoRows): // no env hash yet: fall through and set it
		default:
			return err
		}
		if applied != "" && bcrypt.CompareHashAndPassword([]byte(applied), []byte(username+"\n"+password)) == nil {
			return nil
		}
		if err := d.SetPassword(username, password); err != nil {
			return err
		}
		if _, err := d.db.Exec(`UPDATE users SET role = 'admin' WHERE id = $1`, id); err != nil {
			return err
		}
	}
	h, err := bcrypt.GenerateFromPassword([]byte(username+"\n"+password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = d.db.Exec(`INSERT INTO settings (k, v) VALUES ('admin_env', $1) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, string(h))
	return err
}

type User struct {
	ID       string    `json:"id"`
	Org      string    `json:"org"`
	Username string    `json:"username"`
	Role     string    `json:"role"` // admin: every org; member: own org only
	Created  time.Time `json:"created"`
}

var ErrExists = errors.New("already exists")

// isDuplicate maps a driver's unique-constraint violation to ErrExists. The
// COUNT pre-checks it replaces raced between two API nodes; the constraint is
// the only authority.
func isDuplicate(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == 1555 || se.Code() == 2067 // SQLITE_CONSTRAINT_PRIMARYKEY, SQLITE_CONSTRAINT_UNIQUE
	}
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505" // unique_violation
}

func (d *DB) CreateUser(org, username, password, role string) (*User, error) {
	ph, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	u := &User{ID: randID(12), Org: org, Username: username, Role: role, Created: d.Now().UTC().Truncate(time.Second)}
	_, err = d.db.Exec(`INSERT INTO users (id, org_id, username, password_hash, role, created) VALUES ($1, $2, $3, $4, $5, $6)`,
		u.ID, org, username, string(ph), role, u.Created.Unix())
	if isDuplicate(err) {
		return nil, ErrExists
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ListUsers lists org's users; empty org = every org.
func (d *DB) ListUsers(org string) ([]User, error) {
	rows, err := d.db.Query(`SELECT id, org_id, username, role, created FROM users WHERE $1 = '' OR org_id = $1 ORDER BY created, username`, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var created int64
		if err := rows.Scan(&u.ID, &u.Org, &u.Username, &u.Role, &created); err != nil {
			return nil, err
		}
		u.Created = time.Unix(created, 0).UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUser ends the user's sessions with it. Their API keys stay: keys belong to the org.
func (d *DB) DeleteUser(username string) (bool, error) {
	var id string
	if err := d.db.QueryRow(`SELECT id FROM users WHERE username = $1`, username).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := d.dropSessions(id); err != nil {
		return false, err
	}
	_, err := d.db.Exec(`DELETE FROM users WHERE id = $1`, id)
	return err == nil, err
}

// SetPassword replaces username's password and signs out all of its sessions.
func (d *DB) SetPassword(username, password string) error {
	ph, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	var id string
	if err := d.db.QueryRow(`SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		return err
	}
	if _, err := d.db.Exec(`UPDATE users SET password_hash = $1 WHERE id = $2`, string(ph), id); err != nil {
		return err
	}
	return d.dropSessions(id)
}

type Org struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

func (d *DB) ListOrgs() ([]Org, error) {
	rows, err := d.db.Query(`SELECT id, name, created FROM orgs ORDER BY created, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Org{}
	for rows.Next() {
		var o Org
		var created int64
		if err := rows.Scan(&o.ID, &o.Name, &created); err != nil {
			return nil, err
		}
		o.Created = time.Unix(created, 0).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

func (d *DB) OrgExists(id string) bool {
	var n int
	d.db.QueryRow(`SELECT COUNT(*) FROM orgs WHERE id = $1`, id).Scan(&n)
	return n > 0
}

func (d *DB) dropSessions(userID string) error {
	_, err := d.db.Exec(`DELETE FROM sessions WHERE user_id = $1`, userID)
	d.mu.Lock()
	clear(d.cache) // ponytail: session hashes aren't indexed by user; a password change is rare
	d.mu.Unlock()
	return err
}

// Burned on unknown usernames so a wrong name costs the same bcrypt time as a wrong password.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dawnbx"), bcrypt.DefaultCost)

// CheckPassword returns ErrUnauthorized unless password is username's.
func (d *DB) CheckPassword(username, password string) error {
	_, _, _, err := d.checkPassword(username, password)
	return err
}

func (d *DB) checkPassword(username, password string) (id, org, role string, err error) {
	var ph string
	err = d.db.QueryRow(`SELECT id, password_hash, org_id, role FROM users WHERE username = $1`, username).Scan(&id, &ph, &org, &role)
	if errors.Is(err, sql.ErrNoRows) {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return "", "", "", ErrUnauthorized
	}
	if err != nil {
		return "", "", "", err
	}
	if bcrypt.CompareHashAndPassword([]byte(ph), []byte(password)) != nil {
		return "", "", "", ErrUnauthorized
	}
	return id, org, role, nil
}

// Login returns a session token for the dashboard cookie.
func (d *DB) Login(username, password string) (string, *Principal, error) {
	id, org, role, err := d.checkPassword(username, password)
	if err != nil {
		return "", nil, err
	}
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	now := d.Now()
	d.db.Exec(`DELETE FROM sessions WHERE expires < $1`, now.Unix())
	if _, err := d.db.Exec(`INSERT INTO sessions (hash, user_id, created, expires) VALUES ($1, $2, $3, $4)`,
		hash(tok), id, now.Unix(), now.Add(SessionTTL).Unix()); err != nil {
		return "", nil, err
	}
	return tok, &Principal{Org: org, User: username, Admin: role == "admin"}, nil
}

func (d *DB) CheckSession(tok string) (*Principal, error) {
	if tok == "" {
		return nil, ErrUnauthorized
	}
	h := "s:" + hash(tok)
	if p := d.lookup(h); p != nil {
		return p, nil
	}
	var p Principal
	var role string
	var exp int64
	err := d.db.QueryRow(`SELECT u.username, u.org_id, u.role, s.expires FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.hash = $1`, h[2:]).
		Scan(&p.User, &p.Org, &role, &exp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && d.Now().Unix() >= exp) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	p.Admin = role == "admin"
	d.remember(h, &p)
	return &p, nil
}

func (d *DB) Logout(tok string) error {
	h := hash(tok)
	d.forget("s:" + h)
	_, err := d.db.Exec(`DELETE FROM sessions WHERE hash = $1`, h)
	return err
}

// --- audit ---

type Event struct {
	At     time.Time `json:"at"`
	Org    string    `json:"org"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
}

// Audit records who did what; failures only log, they never fail the request.
func (d *DB) Audit(org, actor, action, target string) error {
	_, err := d.db.Exec(`INSERT INTO audit_log (id, at, org_id, actor, action, target) VALUES ($1, $2, $3, $4, $5, $6)`,
		randID(16), d.Now().Unix(), org, actor, action, target)
	return err
}

// Events lists the newest audit entries first; empty org = every org.
func (d *DB) Events(org string, limit int) ([]Event, error) {
	rows, err := d.db.Query(`SELECT at, org_id, actor, action, target FROM audit_log WHERE $1 = '' OR org_id = $1
		ORDER BY at DESC, id LIMIT $2`, org, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&at, &e.Org, &e.Actor, &e.Action, &e.Target); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- clusters ---

// Cluster is a cluster this control plane runs elsewhere. ProviderState is the
// adapter's opaque handle: it is stored and handed back verbatim, never read
// here, and carries no json tag so no response can leak it. The other names
// are the ones the API contract uses, which is where a Cluster is projected.
type Cluster struct {
	Name          string    `json:"name"`
	Provider      string    `json:"provider"`
	Region        string    `json:"region"`
	InstanceType  string    `json:"instance_type"`
	DiskGiB       int       `json:"disk_gib"`
	Domain        string    `json:"domain"`
	Status        string    `json:"status"`
	Phase         string    `json:"phase"`
	Detail        string    `json:"detail"`
	QuoteID       string    `json:"-"`
	HourlyUSD     float64   `json:"hourly_usd"`
	MonthlyUSD    float64   `json:"monthly_usd"`
	ProviderState string    `json:"-"`
	URL           string    `json:"url"`
	TLSPin        string    `json:"tls_pin"`
	Created       time.Time `json:"created"`
	Updated       time.Time `json:"updated"`
}

// ClusterCredentials is the one response shape in the API that carries
// plaintext. The store holds it sealed and opens it only on the audited route.
type ClusterCredentials struct {
	APIKey        string `json:"api_key"`
	AdminPassword string `json:"admin_password"`
}

// ClusterNode is one worker. ID is the handle the provider returned, not
// something this control plane invented. Sandboxes is a cache of the cluster's
// own answer; the cluster is the authority.
type ClusterNode struct {
	Cluster      string    `json:"-"`
	ID           string    `json:"id"`
	InstanceType string    `json:"instance_type"`
	Status       string    `json:"status"`
	Detail       string    `json:"detail"`
	Sandboxes    int       `json:"sandboxes"`
	Created      time.Time `json:"created"`
}

// Op is one row of provisioning history; the newest for a (cluster, kind) is
// the current phase.
type Op struct {
	ID      string    `json:"-"`
	Cluster string    `json:"-"`
	Kind    string    `json:"kind"`
	Phase   string    `json:"phase"`
	Detail  string    `json:"detail"`
	Created time.Time `json:"created"`
}

const clusterCols = `name, provider, region, instance_type, disk_gib, domain, status, phase, detail,
	quote_id, hourly_usd, monthly_usd, provider_state, url, tls_pin, created, updated`

type scanner interface{ Scan(...any) error }

func (c *Cluster) scan(s scanner) error {
	var created, updated int64
	if err := s.Scan(&c.Name, &c.Provider, &c.Region, &c.InstanceType, &c.DiskGiB, &c.Domain, &c.Status,
		&c.Phase, &c.Detail, &c.QuoteID, &c.HourlyUSD, &c.MonthlyUSD, &c.ProviderState, &c.URL, &c.TLSPin,
		&created, &updated); err != nil {
		return err
	}
	c.Created, c.Updated = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return nil
}

// stamp falls back to the database's clock when a caller left the time unset.
func (d *DB) stamp(t time.Time) time.Time {
	if t.IsZero() {
		return d.Now()
	}
	return t
}

// CreateCluster records a cluster. Callers own the clock: created and updated
// come from the row so the record reads the same as the caller's time.
func (d *DB) CreateCluster(c Cluster) error {
	created, updated := d.stamp(c.Created), d.stamp(c.Updated)
	_, err := d.db.Exec(`INSERT INTO clusters (`+clusterCols+`) VALUES
		($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		c.Name, c.Provider, c.Region, c.InstanceType, c.DiskGiB, c.Domain, c.Status, c.Phase, c.Detail,
		c.QuoteID, c.HourlyUSD, c.MonthlyUSD, c.ProviderState, c.URL, c.TLSPin, created.Unix(), updated.Unix())
	if isDuplicate(err) {
		return ErrExists
	}
	return err
}

// GetCluster returns one cluster, sql.ErrNoRows if it has none.
func (d *DB) GetCluster(name string) (*Cluster, error) {
	c := &Cluster{}
	if err := c.scan(d.db.QueryRow(`SELECT `+clusterCols+` FROM clusters WHERE name = $1`, name)); err != nil {
		return nil, err
	}
	return c, nil
}

// ListClusters returns every cluster, newest first.
func (d *DB) ListClusters() ([]Cluster, error) {
	rows, err := d.db.Query(`SELECT ` + clusterCols + ` FROM clusters ORDER BY created DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cluster{}
	for rows.Next() {
		var c Cluster
		if err := c.scan(rows); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCluster forgets a cluster; its credentials, nodes and ops go with it.
func (d *DB) DeleteCluster(name string) error {
	_, err := d.db.Exec(`DELETE FROM clusters WHERE name = $1`, name)
	return err
}

// SetClusterState records a status transition and the phase it reached, and
// stamps `updated` so a poll can tell a stale row from a live one.
func (d *DB) SetClusterState(name, status, phase, detail string) error {
	_, err := d.db.Exec(`UPDATE clusters SET status = $2, phase = $3, detail = $4, updated = $5 WHERE name = $1`,
		name, status, phase, detail, d.Now().Unix())
	return err
}

// SetClusterURL records the URL and the certificate pin, once the cluster
// answers. It is a change like any other, so `updated` moves too.
func (d *DB) SetClusterURL(name, url, pin string) error {
	_, err := d.db.Exec(`UPDATE clusters SET url = $2, tls_pin = $3, updated = $4 WHERE name = $1`,
		name, url, pin, d.Now().Unix())
	return err
}

// SetProviderState replaces the provider's opaque handle. The DB never looks
// inside it: it is one string column, so nothing here has to know what a
// provider calls its resources, and a second provider needs no migration.
// SetPin records the certificate pin on its own. The pin is needed on every poll
// while a cluster boots, long before it is ready to be handed to an operator.
func (d *DB) SetPin(name, pin string) error {
	_, err := d.db.Exec(`UPDATE clusters SET tls_pin = $2, updated = $3 WHERE name = $1`,
		name, pin, d.Now().Unix())
	return err
}

func (d *DB) SetProviderState(name, state string) error {
	_, err := d.db.Exec(`UPDATE clusters SET provider_state = $2, updated = $3 WHERE name = $1`,
		name, state, d.Now().Unix())
	return err
}

// SaveCredentials stores the sealed credentials for a cluster, replacing any
// it already has. apiEnc is nil until the cluster has minted its own key, and a
// later call carrying only the admin password must not throw that key away.
func (d *DB) SaveCredentials(cluster string, adminEnc, apiEnc []byte) error {
	_, err := d.db.Exec(`INSERT INTO cluster_credentials (cluster, admin_password, api_key, created)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (cluster) DO UPDATE SET admin_password = $2, api_key = COALESCE($3, cluster_credentials.api_key)`,
		cluster, adminEnc, apiEnc, d.Now().Unix())
	return err
}

// Credentials returns the sealed pair. api is nil until the cluster has issued
// its key; rotated is nil until an operator rotated them.
func (d *DB) Credentials(cluster string) (admin, api []byte, rotated *time.Time, err error) {
	var rot sql.NullInt64
	err = d.db.QueryRow(`SELECT admin_password, api_key, rotated FROM cluster_credentials WHERE cluster = $1`, cluster).
		Scan(&admin, &api, &rot)
	if err != nil {
		return nil, nil, nil, err
	}
	return admin, api, ptime(rot), nil
}

// RotateCredentials replaces both secrets and records when. The cluster keeps
// the old pair until an operator applies the new one; that is the caller's
// detail to write, not a fact this store can know.
func (d *DB) RotateCredentials(cluster string, adminEnc, apiEnc []byte) error {
	_, err := d.db.Exec(`UPDATE cluster_credentials SET admin_password = $2, api_key = $3, rotated = $4 WHERE cluster = $1`,
		cluster, adminEnc, apiEnc, d.Now().Unix())
	return err
}

// AddNode records a worker. Recording the same instance twice is a duplicate,
// not a refresh: the caller decides whether to keep or drop the first.
func (d *DB) AddNode(n ClusterNode) error {
	_, err := d.db.Exec(`INSERT INTO cluster_nodes (cluster, id, instance_type, status, detail, sandboxes, created)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		n.Cluster, n.ID, n.InstanceType, n.Status, n.Detail, n.Sandboxes, d.stamp(n.Created).Unix())
	if isDuplicate(err) {
		return ErrExists
	}
	return err
}

// ListNodes returns a cluster's workers, oldest first.
func (d *DB) ListNodes(cluster string) ([]ClusterNode, error) {
	rows, err := d.db.Query(`SELECT cluster, id, instance_type, status, detail, sandboxes, created
		FROM cluster_nodes WHERE cluster = $1 ORDER BY created, id`, cluster)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ClusterNode{}
	for rows.Next() {
		var n ClusterNode
		var created int64
		if err := rows.Scan(&n.Cluster, &n.ID, &n.InstanceType, &n.Status, &n.Detail, &n.Sandboxes, &created); err != nil {
			return nil, err
		}
		n.Created = time.Unix(created, 0).UTC()
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetNodeStatus records where a worker is and how many sandboxes the cluster
// last said it held.
func (d *DB) SetNodeStatus(cluster, id, status, detail string, sandboxes int) error {
	_, err := d.db.Exec(`UPDATE cluster_nodes SET status = $3, detail = $4, sandboxes = $5
		WHERE cluster = $1 AND id = $2`, cluster, id, status, detail, sandboxes)
	return err
}

// DeleteNode forgets one worker; its cluster keeps the rest.
func (d *DB) DeleteNode(cluster, id string) error {
	_, err := d.db.Exec(`DELETE FROM cluster_nodes WHERE cluster = $1 AND id = $2`, cluster, id)
	return err
}

// RecordOp appends one phase of one operation. The table is append-only: the
// history is what makes a failed provisioning explicable afterwards.
func (d *DB) RecordOp(cluster, kind, phase, detail string) error {
	_, err := d.db.Exec(`INSERT INTO cluster_ops (id, cluster, kind, phase, detail, created)
		VALUES ($1, $2, $3, $4, $5, $6)`, randID(16), cluster, kind, phase, detail, d.Now().Unix())
	return err
}

// Ops returns phase history newest first; empty kind is every kind. A limit of
// zero or less means every row.
//
// The limit is applied in Go rather than by a SQL LIMIT clause because SQL reads
// LIMIT 0 as "no rows", which is the opposite of what a caller asking for the
// whole history means. An earlier version passed the limit straight through and
// the cluster registry asked for 0, so the stall check read an empty history in
// production while the test double, which treated 0 as unlimited, said it worked.
func (d *DB) Ops(cluster, kind string, limit int) ([]Op, error) {
	rows, err := d.db.Query(`SELECT id, cluster, kind, phase, detail, created FROM cluster_ops
		WHERE cluster = $1 AND ($2 = '' OR kind = $2) ORDER BY created DESC, id DESC`, cluster, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Op{}
	for rows.Next() {
		var o Op
		var created int64
		if err := rows.Scan(&o.ID, &o.Cluster, &o.Kind, &o.Phase, &o.Detail, &created); err != nil {
			return nil, err
		}
		o.Created = time.Unix(created, 0).UTC()
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
