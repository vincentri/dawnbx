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

	"github.com/jackc/pgx/v5/pgconn"
)

// open returns a fresh SQLite DB, or a Postgres one when DAWNBX_TEST_DATABASE_URL is set
// (the tables are dropped first, so point it at a scratch database).
func open(t *testing.T) *DB {
	url := os.Getenv("DAWNBX_TEST_DATABASE_URL")
	if url == "" {
		return openURL(t, filepath.Join(t.TempDir(), "dawnbx.db"))
	}
	raw, err := sql.Open("pgx", url) // raw: Open would migrate before the drop
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range []string{"cluster_ops", "cluster_nodes", "cluster_credentials", "clusters",
		"audit_log", "sessions", "api_keys", "users", "orgs", "settings", "schema_migrations"} {
		if _, err := raw.Exec(`DROP TABLE IF EXISTS ` + tb); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	return openURL(t, url)
}

// openURL opens url and closes it at the end of the test. Handing it the same
// url twice is how the migration test proves a second Open is a no-op.
func openURL(t *testing.T, url string) *DB {
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

// The SQLite codes are covered by TestUsersOrgs; this pins the Postgres one,
// which no test here can reach without a server.
func TestIsDuplicatePostgres(t *testing.T) {
	if !isDuplicate(&pgconn.PgError{Code: "23505"}) {
		t.Error("unique_violation not read as a duplicate")
	}
	if isDuplicate(&pgconn.PgError{Code: "23503"}) { // foreign_key_violation
		t.Error("foreign key read as a duplicate")
	}
}

func newCluster(name string, at time.Time) Cluster {
	return Cluster{Name: name, Provider: "aws", Region: "us-east-1", InstanceType: "t4g.medium",
		DiskGiB: 60, Status: "provisioning", Phase: "validating", QuoteID: "q-" + name,
		HourlyUSD: 0.0168, MonthlyUSD: 12.26, ProviderState: `{"stack":"dawnbx-` + name + `"}`,
		Created: at, Updated: at}
}

// The four cluster tables migrate onto a fresh database, and a second Open on
// the same file leaves the rows alone.
func TestClusterMigrations(t *testing.T) {
	url := filepath.Join(t.TempDir(), "dawnbx.db")
	d := openURL(t, url)
	now := time.Unix(1_800_000_000, 0)
	d.Now = func() time.Time { return now }
	if err := d.CreateCluster(newCluster("alpha", now)); err != nil {
		t.Fatal(err)
	}

	again := openURL(t, url)
	c, err := again.GetCluster("alpha")
	if err != nil || c.Name != "alpha" {
		t.Fatal(c, err)
	}
	if err := again.CreateCluster(newCluster("beta", now)); err != nil {
		t.Fatal("migration on an existing database:", err)
	}
}

func TestClusters(t *testing.T) {
	d := open(t)
	now := time.Unix(1_800_000_000, 0)
	d.Now = func() time.Time { return now }

	c := newCluster("alpha", now)
	c.Domain = "alpha.example.com"
	if err := d.CreateCluster(c); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateCluster(newCluster("alpha", now)); !errors.Is(err, ErrExists) {
		t.Error("duplicate name", err)
	}

	got, err := d.GetCluster("alpha")
	if err != nil {
		t.Fatal(err)
	}
	want := c
	want.Created, want.Updated = now.UTC(), now.UTC()
	if *got != want {
		t.Errorf("round trip: got %+v want %+v", *got, want)
	}

	// A record written without a timestamp takes the database's clock, so
	// nothing ends up at the zero time.
	d.Now = func() time.Time { return now.Add(time.Hour) }
	if err := d.CreateCluster(newCluster("beta", time.Time{})); err != nil {
		t.Fatal(err)
	}
	if got, err = d.GetCluster("beta"); err != nil || !got.Created.Equal(now.Add(time.Hour)) {
		t.Errorf("unstamped row: %+v %v", got, err)
	}
	d.Now = func() time.Time { return now }

	list, err := d.ListClusters()
	if err != nil || len(list) != 2 || list[0].Name != "beta" {
		t.Fatalf("newest first: %+v %v", list, err)
	}

	now = now.Add(2 * time.Hour)
	if err := d.SetClusterState("alpha", "ready", "", "install finished"); err != nil {
		t.Fatal(err)
	}
	if got, err = d.GetCluster("alpha"); err != nil || got.Status != "ready" ||
		got.Phase != "" || got.Detail != "install finished" || !got.Updated.Equal(now) {
		t.Errorf("state: %+v %v", got, err)
	}

	now = now.Add(time.Hour)
	if err := d.SetClusterURL("alpha", "https://alpha.example.com", "sha256/abc"); err != nil {
		t.Fatal(err)
	}
	if got, err = d.GetCluster("alpha"); err != nil || got.URL != "https://alpha.example.com" ||
		got.TLSPin != "sha256/abc" || !got.Updated.Equal(now) {
		t.Errorf("url: %+v %v", got, err)
	}

	if err := d.DeleteCluster("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetCluster("alpha"); !errors.Is(err, sql.ErrNoRows) {
		t.Error("cluster survived delete", err)
	}
	if err := d.DeleteCluster("alpha"); err != nil {
		t.Error("second delete", err)
	}
}

func TestClusterCredentials(t *testing.T) {
	d := open(t)
	now := time.Unix(1_800_000_000, 0)
	d.Now = func() time.Time { return now }
	if err := d.CreateCluster(newCluster("alpha", now)); err != nil {
		t.Fatal(err)
	}

	// The admin password is written before the host exists; the key arrives
	// only once the cluster has minted one.
	if err := d.SaveCredentials("alpha", []byte("sealed-admin"), nil); err != nil {
		t.Fatal(err)
	}
	admin, api, rotated, err := d.Credentials("alpha")
	if err != nil || string(admin) != "sealed-admin" || api != nil || rotated != nil {
		t.Fatalf("%q %v %v %v", admin, api, rotated, err)
	}

	if err := d.SaveCredentials("alpha", []byte("sealed-admin"), []byte("sealed-key")); err != nil {
		t.Fatal(err)
	}
	if admin, api, _, err = d.Credentials("alpha"); err != nil || api == nil ||
		string(admin) != "sealed-admin" || string(api) != "sealed-key" {
		t.Fatalf("%q %q %v", admin, api, err)
	}

	// Re-saving only the admin password must not throw the key away.
	if err := d.SaveCredentials("alpha", []byte("sealed-admin-2"), nil); err != nil {
		t.Fatal(err)
	}
	if _, api, _, err = d.Credentials("alpha"); err != nil || string(api) != "sealed-key" {
		t.Errorf("key lost on a password-only save: %q %v", api, err)
	}

	now = now.Add(time.Hour)
	if err := d.RotateCredentials("alpha", []byte("new-admin"), []byte("new-key")); err != nil {
		t.Fatal(err)
	}
	admin, api, rotated, err = d.Credentials("alpha")
	if err != nil || string(admin) != "new-admin" || string(api) != "new-key" ||
		rotated == nil || !rotated.Equal(now) {
		t.Errorf("rotation: %q %q %v %v", admin, api, rotated, err)
	}

	if err := d.DeleteCluster("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.Credentials("alpha"); !errors.Is(err, sql.ErrNoRows) {
		t.Error("credentials outlived the cluster", err)
	}
}

func TestClusterNodesAndOps(t *testing.T) {
	d := open(t)
	now := time.Unix(1_800_000_000, 0)
	d.Now = func() time.Time { now = now.Add(time.Second); return now }
	if err := d.CreateCluster(newCluster("alpha", time.Unix(1_800_000_000, 0))); err != nil {
		t.Fatal(err)
	}
	// The foreign key is what stops a node or an op row for a cluster that
	// was never created.
	if err := d.AddNode(ClusterNode{Cluster: "ghost", ID: "i-1"}); err == nil {
		t.Error("node for a cluster that does not exist")
	}
	if err := d.RecordOp("ghost", "create", "validating", ""); err == nil {
		t.Error("op for a cluster that does not exist")
	}

	for _, id := range []string{"i-1", "i-2"} {
		if err := d.AddNode(ClusterNode{Cluster: "alpha", ID: id, InstanceType: "t4g.medium",
			Status: "provisioning", Detail: "launching"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AddNode(ClusterNode{Cluster: "alpha", ID: "i-1"}); !errors.Is(err, ErrExists) {
		t.Error("duplicate node", err)
	}
	nodes, err := d.ListNodes("alpha")
	if err != nil || len(nodes) != 2 || nodes[0].ID != "i-1" || nodes[0].InstanceType != "t4g.medium" {
		t.Fatalf("nodes: %+v %v", nodes, err)
	}
	if nodes[0].Created.IsZero() || nodes[0].Sandboxes != 0 {
		t.Errorf("node defaults: %+v", nodes[0])
	}

	if err := d.SetNodeStatus("alpha", "i-1", "ready", "", 3); err != nil {
		t.Fatal(err)
	}
	if nodes, err = d.ListNodes("alpha"); err != nil || nodes[0].Status != "ready" || nodes[0].Sandboxes != 3 {
		t.Errorf("node status: %+v %v", nodes, err)
	}

	for _, phase := range []string{"requesting_host", "bootstrapping", "ready"} {
		if err := d.RecordOp("alpha", "create", phase, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.RecordOp("alpha", "add_node", "requesting_host", ""); err != nil {
		t.Fatal(err)
	}
	ops, err := d.Ops("alpha", "create", 10)
	if err != nil || len(ops) != 3 {
		t.Fatalf("ops: %+v %v", ops, err)
	}
	if ops[0].Phase != "ready" || ops[1].Phase != "bootstrapping" || ops[2].Phase != "requesting_host" {
		t.Errorf("not newest first: %+v", ops)
	}
	if ops[0].Cluster != "alpha" || ops[0].ID == "" || ops[0].Created.IsZero() {
		t.Errorf("op row: %+v", ops[0])
	}
	if ops, err = d.Ops("alpha", "create", 2); err != nil || len(ops) != 2 || ops[0].Phase != "ready" {
		t.Errorf("limit: %+v %v", ops, err)
	}
	if ops, err = d.Ops("alpha", "", 10); err != nil || len(ops) != 4 {
		t.Errorf("every kind: %+v %v", ops, err)
	}

	if err := d.DeleteNode("alpha", "i-2"); err != nil {
		t.Fatal(err)
	}
	if nodes, err = d.ListNodes("alpha"); err != nil || len(nodes) != 1 || nodes[0].ID != "i-1" {
		t.Errorf("after delete: %+v %v", nodes, err)
	}

	// Deleting the cluster takes its workers and its phase history with it.
	if err := d.DeleteCluster("alpha"); err != nil {
		t.Fatal(err)
	}
	if nodes, err = d.ListNodes("alpha"); err != nil || len(nodes) != 0 {
		t.Errorf("nodes outlived the cluster: %+v %v", nodes, err)
	}
	if ops, err = d.Ops("alpha", "", 10); err != nil || len(ops) != 0 {
		t.Errorf("ops outlived the cluster: %+v %v", ops, err)
	}
}
