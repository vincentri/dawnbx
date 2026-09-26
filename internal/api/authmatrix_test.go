package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"dawnbx/internal/sandbox"
	"dawnbx/internal/store"
)

// loginCookie signs in as an existing dashboard user and returns the
// Cookie header value to replay.
func loginCookie(t *testing.T, do func(method, path, body string, hdr ...string) *httptest.ResponseRecorder, user, pw string) []string {
	t.Helper()
	w := do("POST", "/v1/login", `{"username":"`+user+`","password":"`+pw+`"}`, "X-Dawnbx", "1")
	if w.Code != 200 {
		t.Fatalf("login %s: %d %s", user, w.Code, w.Body)
	}
	c := w.Result().Cookies()[0]
	return []string{"Cookie", c.Name + "=" + c.Value}
}

// envelope reads an error body and checks its shape: a fixed key set, and
// never the internal status code.
func envelope(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %q", w.Body)
	}
	want := map[string]bool{"code": true, "message": true}
	if hint, ok := got["hint"]; ok {
		if s, _ := hint.(string); s != "" {
			want["hint"] = true
		} else {
			t.Errorf("hint is not a string: %v", hint)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("unexpected field %q in the error envelope %s", k, w.Body)
		}
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q in the error envelope %s", k, w.Body)
		}
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("error Content-Type %q", ct)
	}
	return got
}

// TestAuthMatrix: one table for the three ways a request says who it is, and
// for the rule that keeps a cookie from being used cross-site.
func TestAuthMatrix(t *testing.T) {
	m, st := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{M: m, Auth: db}).Handler()
	do := doer(h, false)
	admin := loginCookie(t, do, "admin", "admin-password")
	if err := db.CreateOrg("acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	bad, _, err := db.CreateKey("acme", "acme", "user:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	dead, deadKey, err := db.CreateKey("acme", "gone", "user:admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := db.RevokeKey("acme", deadKey.ID); !ok {
		t.Fatal("revoke")
	}

	// Only a live bearer token in the Authorization header, or the same token
	// as the "bearer.<key>" WebSocket subprotocol, opens a route.
	for _, c := range []struct {
		name   string
		hdr    []string
		status int
		code   string
	}{
		{"nothing", nil, 401, "unauthorized"},
		{"bearer", []string{"Authorization", "Bearer dawnbx_good"}, 200, ""},
		{"lowercase scheme", []string{"Authorization", "bearer dawnbx_good"}, 200, ""},
		{"UPPERCASE scheme", []string{"Authorization", "BEARER dawnbx_good"}, 200, ""},
		{"other scheme", []string{"Authorization", "Basic ZGVtb255"}, 401, "unauthorized"},
		{"empty bearer", []string{"Authorization", "Bearer "}, 401, "unauthorized"},
		{"wrong key", []string{"Authorization", "Bearer dawnbx_nope"}, 401, "unauthorized"},
		{"subprotocol", []string{"Sec-Websocket-Protocol", "dawnbx, bearer.dawnbx_good"}, 200, ""},
		{"subprotocol wrong key", []string{"Sec-Websocket-Protocol", "bearer.dawnbx_nope"}, 401, "unauthorized"},
		{"subprotocol empty key", []string{"Sec-Websocket-Protocol", "bearer."}, 401, "unauthorized"},
		{"cookie", admin, 200, ""},
		{"cookie is dead", []string{"Cookie", "dawnbx_session=nope"}, 401, "unauthorized"},
		// The header is authoritative: a dead key in it is not rescued by a
		// live cookie beside it, or a caller would keep a revoked key working.
		{"dead key beats cookie", []string{"Authorization", "Bearer " + dead, "Cookie", admin[1]}, 401, "unauthorized"},
	} {
		w := do("GET", "/v1/me", "", c.hdr...)
		if w.Code != c.status {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		if c.code != "" {
			if got := envelope(t, w); got["code"] != c.code {
				t.Errorf("%s: code %v, want %s", c.name, got["code"], c.code)
			}
		}
	}

	// The session cookie is the dashboard's own credential, and the
	// X-Dawnbx header is what makes a write provably same-site.
	w := do("GET", "/v1/me", "", admin...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"admin":true`) {
		t.Errorf("admin cookie: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct {
		name   string
		x      string
		status int
		code   string
	}{
		{"write without the header", "", 403, "forbidden"},
		{"write with the wrong header", "0", 403, "forbidden"},
		{"write with the header", "1", 200, ""},
	} {
		w := do("POST", "/v1/orgs", `{"id":"beta"}`, "Cookie", admin[1], "X-Dawnbx", c.x)
		if w.Code != c.status {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		if c.code != "" {
			if got := envelope(t, w); got["code"] != c.code {
				t.Errorf("%s: code %v", c.name, got["code"])
			}
		}
	}
	// The CSRF rule belongs to cookie auth only: a bearer client has no cookie
	// to ride on, so it needs no header.
	w = do("POST", "/v1/keys", `{"name":"via-bearer"}`, "Authorization", "Bearer dawnbx_good")
	if w.Code != 403 || !strings.Contains(w.Body.String(), "API keys can't manage keys or users") {
		t.Errorf("bearer POST /v1/keys: %d %s", w.Code, w.Body)
	}
	w = do("POST", "/v1/sandboxes/"+store.NewID()+"/extend", `{"ttl":"1h"}`, "Authorization", "Bearer dawnbx_good")
	if w.Code != 404 {
		t.Errorf("bearer write without X-Dawnbx: %d %s", w.Code, w.Body)
	}

	// A key from another org is not merely unlisted, it is invisible: the id
	// is not in the list and a direct fetch says not_found.
	ours, theirs := store.NewID(), store.NewID()
	st.Create(store.Meta{ID: ours, Image: "x", Created: m.Now(), Status: sandbox.StatusRunning})
	st.Create(store.Meta{ID: theirs, Image: "x", Created: m.Now(), Status: sandbox.StatusRunning, Org: "acme"})
	w = do("GET", "/v1/sandboxes", "", "Sec-Websocket-Protocol", "bearer."+bad)
	if !strings.Contains(w.Body.String(), theirs) || strings.Contains(w.Body.String(), ours) {
		t.Errorf("acme list: %s", w.Body)
	}
	if w := do("GET", "/v1/sandboxes/"+ours, "", "Sec-Websocket-Protocol", "bearer."+bad); w.Code != 404 {
		t.Errorf("cross-org fetch: %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/v1/sandboxes/"+ours, "", "Authorization", "Bearer "+bad); w.Code != 404 {
		t.Errorf("cross-org kill: %d %s", w.Code, w.Body)
	}
	// Admins are not org-scoped: the same cookie sees both.
	w = do("GET", "/v1/sandboxes", "", admin...)
	if !strings.Contains(w.Body.String(), ours) || !strings.Contains(w.Body.String(), theirs) {
		t.Errorf("admin list: %s", w.Body)
	}
}

// TestDeadAuthDatabaseIsNotUnauthorized: when the auth database fails the
// answer has to be a server error. A 401 would tell the caller to go rotate a
// key that is perfectly fine.
func TestDeadAuthDatabaseIsNotUnauthorized(t *testing.T) {
	m, _ := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{M: m, Auth: db}).Handler()
	do := doer(h, true)
	admin := loginCookie(t, do, "admin", "admin-password")
	db.Close()

	for _, c := range []struct {
		name, method, path, body string
		hdr                      []string
	}{
		{"session", "GET", "/v1/me", "", admin},
		{"key", "GET", "/v1/me", "", []string{"Authorization", "Bearer dawnbx_good"}},
		{"login", "POST", "/v1/login", `{"username":"admin","password":"admin-password"}`, nil},
	} {
		w := do(c.method, c.path, c.body, c.hdr...)
		if w.Code != 500 {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		got := envelope(t, w)
		if got["code"] != "internal" || !strings.Contains(got["hint"].(string), "journalctl") {
			t.Errorf("%s: %s", c.name, w.Body)
		}
	}
}

// TestErrorBodiesAreClientErrors: every refusal the API makes on purpose is a
// 4xx with a code the SDK can branch on, and the message never leaks internals.
func TestErrorBodiesAreClientErrors(t *testing.T) {
	m, st := testManager(t)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	h := (&Server{M: m, Auth: db}).Handler()
	do := doer(h, true)
	admin := loginCookie(t, do, "admin", "admin-password")
	id := store.NewID()
	st.Create(store.Meta{ID: id, Image: "x", Created: m.Now(), Status: sandbox.StatusRunning})

	for _, c := range []struct {
		name, method, path, body string
		hdr                      []string
		status                   int
		code                     string
		hint                     bool
	}{
		{"unknown route", "GET", "/v1/nope", "", nil, 404, "not_found", true},
		{"unknown method", "PUT", "/v1/me", "", admin, 404, "not_found", true},
		{"no key", "GET", "/v1/me", "", nil, 401, "unauthorized", true},
		{"missing sandbox", "GET", "/v1/sandboxes/" + store.NewID(), "", admin, 404, "not_found", true},
		{"bad json", "POST", "/v1/login", `{"username":`, nil, 400, "invalid_request", false},
		{"empty body", "POST", "/v1/orgs", ``, admin, 400, "invalid_request", true},
		{"short password", "POST", "/v1/me/password", `{"old":"admin-password","new":"short"}`, admin, 400, "invalid_request", false},
		{"wrong old password", "POST", "/v1/me/password", `{"old":"wrong-password","new":"a-long-enough-one"}`, admin, 403, "forbidden", false},
		{"bad org id", "POST", "/v1/orgs", `{"id":"Not An Org"}`, admin, 400, "invalid_request", true},
		{"key with no name", "POST", "/v1/keys", `{}`, admin, 400, "invalid_request", true},
	} {
		w := do(c.method, c.path, c.body, c.hdr...)
		if w.Code != c.status {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		got := envelope(t, w)
		if got["code"] != c.code {
			t.Errorf("%s: code %v, want %s", c.name, got["code"], c.code)
		}
		if msg, _ := got["message"].(string); msg == "" || strings.Contains(msg, "sql") || strings.Contains(msg, ".go:") {
			t.Errorf("%s: message %q", c.name, msg)
		}
		if _, ok := got["hint"]; ok != c.hint {
			t.Errorf("%s: hint present=%v, want %v (%s)", c.name, ok, c.hint, w.Body)
		}
	}
}

// TestDashboardStaticSurface: the UI is served from the embedded filesystem
// with the headers a session-cookie dashboard needs, and client-side routes
// fall back to the app shell.
func TestDashboardStaticSurface(t *testing.T) {
	h := (&Server{Auth: testDB(t)}).Handler()
	for _, c := range []struct{ path, body string }{
		{"/", "<title>dawnbx</title>"},
		{"/ui/", `id="root"`},
		{"/ui/settings", `id="root"`}, // client-side route
	} {
		w := doer(h, false)("GET", c.path, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s: %d %s", c.path, w.Code, w.Body)
			continue
		}
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") ||
			!strings.Contains(csp, "form-action 'none'") {
			t.Errorf("%s: CSP %q", c.path, csp)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", c.path, w.Header())
		}
	}
	// A hashed asset name may be cached forever.
	w := doer(h, false)("GET", "/ui/assets/index.js", "")
	if w.Code == 200 && !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("assets without an immutable cache header: %v", w.Header())
	}
	// A path that walks out of the embedded filesystem is refused, not served.
	if w := doer(h, false)("GET", "/ui/assets/../../etc/passwd", ""); w.Code == 200 || strings.Contains(w.Body.String(), "root:") {
		t.Errorf("static handler escaped the embedded filesystem: %d %s", w.Code, w.Body)
	}
}

// TestRouteNotFoundIsExplicit: a GET on a write route is not silently served as
// something else, and a websocket route is not served over plain HTTP.
func TestRouteNotFoundIsExplicit(t *testing.T) {
	m, _ := testManager(t)
	h := (&Server{M: m, Auth: testDB(t)}).Handler()
	if w := doer(h, true)("GET", "/v1/login", ""); w.Code != 404 {
		t.Errorf("GET /v1/login: %d %s", w.Code, w.Body)
	}
	if w := doer(h, true)("GET", "/v1/orgs", ""); w.Code != 401 {
		t.Errorf("unauthenticated admin route: %d", w.Code)
	}
	w := doer(h, false)("GET", "/v1/sandboxes/x/terminal", "", "Authorization", "Bearer dawnbx_good")
	if w.Code == 200 {
		t.Error("terminal route served without an upgrade")
	}
}
