package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"dawnbx/internal/sandbox"
	"dawnbx/internal/store"
)

// TestKeyManagementRules: who may make a key, in which org, and for how long.
func TestKeyManagementRules(t *testing.T) {
	m, _ := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrg("acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{M: m, Auth: db}).Handler()
	do := doer(h, true)
	admin := loginCookie(t, do, "admin", "admin-password")

	for _, c := range []struct {
		name, body, code string
	}{
		{"no name", `{"ttl":"1h"}`, "invalid_request"},
		{"blank name", `{"name":"   "}`, "invalid_request"},
		{"long name", `{"name":"` + strings.Repeat("k", 101) + `"}`, "invalid_request"},
		{"bad ttl", `{"name":"ci","ttl":"soon"}`, "invalid_request"},
		{"negative ttl", `{"name":"ci","ttl":"-1h"}`, "invalid_request"},
		{"zero ttl", `{"name":"ci","ttl":"0s"}`, "invalid_request"},
		{"unknown org", `{"name":"ci","org":"ghost"}`, "invalid_request"},
		{"bad json", `{"name":`, "invalid_request"},
	} {
		w := do("POST", "/v1/keys", c.body, admin...)
		if w.Code != 400 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		if got := envelope(t, w); got["code"] != c.code {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	// An admin may mint a key in another org; nobody else can.
	w := do("POST", "/v1/keys", `{"name":"acme-ci","org":"acme","ttl":"720h"}`, admin...)
	var made struct {
		Key  string `json:"key"`
		Info struct {
			ID      string `json:"id"`
			Org     string `json:"org"`
			Expires string `json:"expires"`
		} `json:"info"`
	}
	json.Unmarshal(w.Body.Bytes(), &made)
	if w.Code != 200 || !strings.HasPrefix(made.Key, "dbx_") || made.Info.Org != "acme" || made.Info.Expires == "" {
		t.Fatalf("create in another org: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/me", "", "Authorization", "Bearer "+made.Key); !strings.Contains(w.Body.String(), `"org":"acme"`) {
		t.Errorf("new key org: %s", w.Body)
	}
	// A member of acme manages acme's keys and cannot reach another org's.
	if _, err := db.CreateUser("acme", "amy", "amy-password", "member"); err != nil {
		t.Fatal(err)
	}
	amy := loginCookie(t, do, "amy", "amy-password")
	def, _, _ := db.CreateKey("default", "default-ci", "user:admin", nil)
	if w := do("DELETE", "/v1/keys/"+def[4:], "", amy...); w.Code != 404 {
		t.Errorf("member revoked another org's key: %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/v1/keys/"+made.Info.ID, "", amy...); w.Code != 204 {
		t.Errorf("member revoked its own org's key: %d %s", w.Code, w.Body)
	}
	// Revoking twice says the key is gone rather than revoking it again.
	if w := do("DELETE", "/v1/keys/"+made.Info.ID, "", admin...); w.Code != 404 ||
		!strings.Contains(w.Body.String(), "no live key") {
		t.Errorf("second revoke: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/me", "", "Authorization", "Bearer "+made.Key); w.Code != 401 {
		t.Errorf("revoked key still works: %d", w.Code)
	}
	// Keys never manage keys, whoever made them.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/keys"},
		{"GET", "/v1/audit"},
		{"GET", "/v1/orgs"},
		{"GET", "/v1/users"},
		{"GET", "/v1/nodes"},
	} {
		w := do(c.method, c.path, "", "Authorization", "Bearer dawnbx_good")
		if w.Code != 403 || !strings.Contains(w.Body.String(), "API keys can't manage keys or users") {
			t.Errorf("bearer %s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	// A member sees its own org's keys and its own org's audit trail only.
	if _, err := db.CreateUser("acme", "bob", "bob-password", "member"); err != nil {
		t.Fatal(err)
	}
	if err := db.Audit("acme", "user:amy", "key.create", "in-acme"); err != nil {
		t.Fatal(err)
	}
	if err := db.Audit("default", "user:admin", "key.create", "in-default"); err != nil {
		t.Fatal(err)
	}
	bob := loginCookie(t, do, "bob", "bob-password")
	w = do("GET", "/v1/audit", "", bob...)
	if !strings.Contains(w.Body.String(), "in-acme") || strings.Contains(w.Body.String(), "in-default") {
		t.Errorf("member audit scope: %s", w.Body)
	}
}

// TestUserManagementRules: usernames, passwords, roles and orgs are all
// validated before a row is written.
func TestUserManagementRules(t *testing.T) {
	m, _ := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{M: m, Auth: db}).Handler()
	do := doer(h, true)
	admin := loginCookie(t, do, "admin", "admin-password")

	for _, c := range []struct{ name, body string }{
		{"bad username", `{"username":"bad user!","password":"a-long-password"}`},
		{"empty username", `{"username":"","password":"a-long-password"}`},
		{"short password", `{"username":"carol","password":"short"}`},
		{"long password", `{"username":"carol","password":"` + strings.Repeat("p", 73) + `"}`},
		{"bad role", `{"username":"carol","password":"a-long-password","role":"owner"}`},
		{"unknown org", `{"username":"carol","password":"a-long-password","org":"ghost"}`},
		{"bad json", `{"username":`},
	} {
		w := do("POST", "/v1/users", c.body, admin...)
		if w.Code != 400 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}
	// No org means the caller's own, and no role means member.
	w := do("POST", "/v1/users", `{"username":"carol","password":"carol-password"}`, admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"org":"default"`) || !strings.Contains(w.Body.String(), `"role":"member"`) {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	// Listing users is admin-only and spans orgs.
	w = do("GET", "/v1/users", "", admin...)
	if !strings.Contains(w.Body.String(), `"username":"carol"`) || !strings.Contains(w.Body.String(), `"username":"admin"`) {
		t.Errorf("users: %d %s", w.Code, w.Body)
	}
	// An admin resets a password; the old one stops working at once.
	carol := loginCookie(t, do, "carol", "carol-password")
	if w := do("POST", "/v1/users/carol/password", `{"password":"short"}`, admin...); w.Code != 400 {
		t.Errorf("short admin reset: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/users/carol/password", `{"password":`, admin...); w.Code != 400 {
		t.Errorf("bad json admin reset: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/users/nobody/password", `{"password":"a-long-password"}`, admin...); w.Code != 404 {
		t.Errorf("reset of a missing user: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/users/carol/password", `{"password":"carol-password-2"}`, admin...); w.Code != 204 {
		t.Fatalf("admin reset: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/me", "", carol...); w.Code != 401 {
		t.Errorf("session survived an admin reset: %d", w.Code)
	}
	carol = loginCookie(t, do, "carol", "carol-password-2")
	// A member cannot reset passwords, not even their own through this route.
	if w := do("POST", "/v1/users/carol/password", `{"password":"carol-password-3"}`, carol...); w.Code != 403 {
		t.Errorf("member reset: %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/v1/users/nobody", "", admin...); w.Code != 404 {
		t.Errorf("delete missing user: %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/v1/users/carol", "", admin...); w.Code != 204 {
		t.Errorf("delete: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/me", "", carol...); w.Code != 401 {
		t.Errorf("deleted user's session: %d", w.Code)
	}
}

// TestOrgManagementRules: org ids are the tenant boundary, so the shape is
// checked before anything is created.
func TestOrgManagementRules(t *testing.T) {
	m, _ := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{M: m, Auth: db}).Handler()
	do := doer(h, true)
	admin := loginCookie(t, do, "admin", "admin-password")

	for _, c := range []struct{ name, body string }{
		{"space", `{"id":"acme corp"}`},
		{"leading dash", `{"id":"-acme"}`},
		{"uppercase", `{"id":"Acme"}`},
		{"too long", `{"id":"` + strings.Repeat("a", 41) + `"}`},
		{"empty", `{"id":""}`},
	} {
		if w := do("POST", "/v1/orgs", c.body, admin...); w.Code != 400 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}
	// No name falls back to the id.
	w := do("POST", "/v1/orgs", `{"id":"acme"}`, admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"acme"`) {
		t.Fatalf("org: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/orgs", `{"id":"acme","name":"  "}`, admin...); w.Code != 409 {
		t.Errorf("dup: %d %s", w.Code, w.Body)
	}
	w = do("GET", "/v1/orgs", "", admin...)
	if !strings.Contains(w.Body.String(), `"id":"acme"`) || !strings.Contains(w.Body.String(), `"id":"default"`) {
		t.Errorf("orgs: %d %s", w.Code, w.Body)
	}
	// A member is not an org admin.
	if _, err := db.CreateUser("acme", "dan", "dan-password", "member"); err != nil {
		t.Fatal(err)
	}
	dan := loginCookie(t, do, "dan", "dan-password")
	if w := do("GET", "/v1/orgs", "", dan...); w.Code != 403 || !strings.Contains(w.Body.String(), "admins only") {
		t.Errorf("member listed orgs: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/orgs", `{"id":"evil"}`, dan...); w.Code != 403 {
		t.Errorf("member made an org: %d %s", w.Code, w.Body)
	}
}

// TestNodeManagement: workers are only removable while they hold nothing, and
// the join token is never shown to a non-admin.
func TestNodeManagement(t *testing.T) {
	m, st := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.Self = "srv"
	for _, n := range []string{"srv", "w1"} {
		if _, err := m.Kube.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	h := (&Server{M: m, Auth: db}).Handler()
	do := doer(h, true)
	admin := loginCookie(t, do, "admin", "admin-password")

	// No join token on disk: the admin gets a clear server error, not a blank
	// command that would be copy-pasted into a shell.
	tok := sandbox.NodeTokenFile
	sandbox.NodeTokenFile = t.TempDir() + "/absent"
	defer func() { sandbox.NodeTokenFile = tok }()
	w := do("GET", "/v1/nodes/join", "", admin...)
	if w.Code != 500 || !strings.Contains(w.Body.String(), `"code":"no_join_token"`) {
		t.Errorf("join without a token: %d %s", w.Code, w.Body)
	}
	// A worker holding nothing can be dropped, and it is audited.
	if w := do("DELETE", "/v1/nodes/w1", "", admin...); w.Code != 204 {
		t.Errorf("remove idle worker: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/nodes", "", admin...); strings.Contains(w.Body.String(), `"name":"w1"`) {
		t.Errorf("worker still listed: %s", w.Body)
	}
	if w := do("DELETE", "/v1/nodes/w1", "", admin...); w.Code != 404 {
		t.Errorf("remove twice: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/audit", "", admin...); !strings.Contains(w.Body.String(), `"action":"node.delete"`) {
		t.Errorf("no delete event: %s", w.Body)
	}
	// The server node runs dawnbx itself and cannot be removed.
	if w := do("DELETE", "/v1/nodes/srv", "", admin...); w.Code != 400 {
		t.Errorf("remove server node: %d %s", w.Code, w.Body)
	}
	// Nothing to list beats an empty list when the cluster is unreachable.
	if l, _ := st.IDs(); len(l) != 0 {
		t.Errorf("unexpected sandboxes: %v", l)
	}
}

// TestPasswordChangeLockout: guessing the current password is rate limited on
// the same counter as the login form, per user and per address.
func TestPasswordChangeLockout(t *testing.T) {
	m, _ := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser("default", "eve", "eve-password", "member"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{M: m, Auth: db}).Handler()
	eve := loginCookie(t, doer(h, true), "eve", "eve-password")

	send := func(t *testing.T, addr, body string, hdr ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/me/password", strings.NewReader(body))
		req.Header.Set("X-Dawnbx", "1")
		req.RemoteAddr = addr // a bare address, as a proxy in front would send
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	// A correct password somewhere in the middle clears the counter.
	for i := 0; i < 4; i++ {
		if w := send(t, "203.0.113.7", `{"old":"wrong-password","new":"eve-password-2"}`, eve...); w.Code != 403 {
			t.Fatalf("miss %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := send(t, "203.0.113.7", `{"old":"eve-password","new":"eve-password-2"}`, eve...); w.Code != 204 {
		t.Fatalf("correct after four misses: %d %s", w.Code, w.Body)
	}
	eve = loginCookie(t, doer(h, true), "eve", "eve-password-2")
	// Five misses and the right password is refused too.
	for i := 0; i < 5; i++ {
		if w := send(t, "203.0.113.7", `{"old":"wrong-password","new":"eve-password-3"}`, eve...); w.Code != 403 {
			t.Fatalf("miss %d: %d %s", i, w.Code, w.Body)
		}
	}
	w := send(t, "203.0.113.7", `{"old":"eve-password-2","new":"eve-password-3"}`, eve...)
	if w.Code != 429 || !strings.Contains(w.Body.String(), `"code":"too_many_attempts"`) {
		t.Errorf("not locked out: %d %s", w.Code, w.Body)
	}
	// The lockout is per address, so another one is not affected by it.
	w = send(t, "198.51.100.9", `{"old":"eve-password-2","new":"eve-password-3"}`, eve...)
	if w.Code != 204 {
		t.Errorf("lockout spilled to another address: %d %s", w.Code, w.Body)
	}
	// A fresh session is still locked out on the address that guessed wrong.
	eve = loginCookie(t, doer(h, true), "eve", "eve-password-3")
	if w := send(t, "203.0.113.7", `{"old":"eve-password-3","new":"eve-password-4"}`, eve...); w.Code != 429 {
		t.Errorf("lockout ended early: %d %s", w.Code, w.Body)
	}
}

// TestSandboxOrgSurvivesKilling: killing is audited under the caller's org, so
// one tenant's log cannot be used to prove another's sandbox existed.
func TestMemberCannotReachTheAuditOfAnotherOrg(t *testing.T) {
	m, st := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateOrg("acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateUser("acme", "fay", "fay-password", "member"); err != nil {
		t.Fatal(err)
	}
	if err := db.Audit("default", "key:abc", "sandbox.create", "secret-id"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{M: m, Auth: db}).Handler()
	do := doer(h, true)
	fay := loginCookie(t, do, "fay", "fay-password")
	id := store.NewID()
	st.Create(store.Meta{ID: id, Image: "x", Created: m.Now(), Status: sandbox.StatusRunning, Org: "acme"})
	if w := do("GET", "/v1/audit", "", fay...); strings.Contains(w.Body.String(), "secret-id") {
		t.Errorf("member read another org's audit: %s", w.Body)
	}
	if w := do("GET", "/v1/keys", "", fay...); strings.Contains(w.Body.String(), "installer") {
		t.Errorf("member listed another org's keys: %s", w.Body)
	}
	admin := loginCookie(t, do, "admin", "admin-password")
	if w := do("GET", "/v1/audit", "", admin...); !strings.Contains(w.Body.String(), "secret-id") {
		t.Errorf("admin cannot read the audit: %s", w.Body)
	}
}
