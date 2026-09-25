package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/remotecommand"

	"dawnbx/internal/auth"
	"dawnbx/internal/sandbox"
	"dawnbx/internal/store"
)

// testDB is a SQLite auth DB where "dawnbx_good" is the installer key.
func testDB(t *testing.T) *auth.DB {
	dir := t.TempDir()
	db, err := auth.Open(filepath.Join(dir, "dawnbx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sum := sha256.Sum256([]byte("dawnbx_good"))
	path := filepath.Join(dir, "api-keys.json")
	os.WriteFile(path, []byte(`{"v":1,"keys":[{"sha256":"`+hex.EncodeToString(sum[:])+`"}]}`), 0o600)
	if err := db.ImportKeyFile(path); err != nil {
		t.Fatal(err)
	}
	return db
}

func testManager(t *testing.T) (*sandbox.Manager, *store.Store) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, store.Marker), []byte("x"), 0o600)
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.New(st, fake.NewClientset(), nil), st
}

func TestAuthAndEnvelope(t *testing.T) {
	h := (&Server{Auth: testDB(t)}).Handler()

	for _, c := range []struct {
		method, path, key string
		status            int
		body              string
	}{
		{"GET", "/v1/version", "", 200, `"api":"v1"`},
		{"GET", "/v1/sandboxes", "", 401, `"code":"unauthorized"`},
		{"GET", "/v1/sandboxes", "dawnbx_bad", 401, `"hint":"set DAWNBX_API_KEY`},
		{"GET", "/nope", "dawnbx_good", 404, `"code":"not_found"`},
		{"GET", "/", "", 200, "<title>dawnbx</title>"},
		{"GET", "/ui/settings", "", 200, `id="root"`},
		{"GET", "/ui/assets/nope.js", "", 404, ""},
		{"GET", "/v1/status", "", 401, `"code":"unauthorized"`},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.body) {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		if c.path == "/" && !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("dashboard served without CSP: %v", w.Header())
		}
	}
}

func TestTerminal(t *testing.T) {
	m, st := testManager(t)
	id := store.NewID()
	st.Create(store.Meta{ID: id, Image: "python:3.12-slim", Created: time.Now(), Status: sandbox.StatusRunning})
	// Fake shell: reports the first resize, then echoes lines until "exit".
	m.RunTTY = func(ctx context.Context, _ string, _ []string, in io.Reader, out io.Writer, sizes remotecommand.TerminalSizeQueue) (int, error) {
		sz := sizes.Next()
		fmt.Fprintf(out, "size %dx%d\n", sz.Width, sz.Height)
		sc := bufio.NewScanner(in)
		for sc.Scan() && sc.Text() != "exit" {
			fmt.Fprintf(out, "echo %s\n", sc.Text())
		}
		return 0, nil
	}
	srv := httptest.NewServer((&Server{M: m, Auth: testDB(t)}).Handler())
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/sandboxes/"
	dial := func(id, key, origin string) (*websocket.Conn, *http.Response, error) {
		d := websocket.Dialer{Subprotocols: []string{"dawnbx", "bearer." + key}}
		h := http.Header{}
		if origin != "" {
			h.Set("Origin", origin)
		}
		return d.Dial(url+id+"/terminal", h)
	}

	if _, res, err := dial(id, "dawnbx_bad", ""); err == nil || res.StatusCode != 401 {
		t.Fatalf("bad key: %v %v", err, res)
	}
	if _, res, err := dial(id, "dawnbx_good", "https://evil.example"); err == nil || res.StatusCode != 403 {
		t.Fatalf("cross-origin: %v %v", err, res)
	}

	c, _, err := dial(id, "dawnbx_good", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subprotocol() != "dawnbx" {
		t.Errorf("subprotocol %q", c.Subprotocol())
	}
	c.WriteMessage(websocket.TextMessage, []byte(`{"cols":120,"rows":40}`))
	c.WriteMessage(websocket.BinaryMessage, []byte("hi\nexit\n"))
	var got string
	for {
		_, b, err := c.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				t.Errorf("close: %v", err)
			}
			break
		}
		got += string(b)
	}
	if got != "size 120x40\necho hi\n" {
		t.Errorf("output %q", got)
	}

	if _, res, err := dial(store.NewID(), "dawnbx_good", ""); err == nil || res.StatusCode != 404 {
		t.Errorf("missing sandbox: %v %v", err, res)
	}
}

func TestOrgsSessionsKeys(t *testing.T) {
	m, st := testManager(t)
	db := testDB(t)
	if err := db.CreateOrg("acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	acme, _, _ := db.CreateKey("acme", "acme-ci", "test", nil)
	db.EnsureAdmin("admin", "pw")
	h := (&Server{M: m, Auth: db}).Handler()
	mine, theirs := store.NewID(), store.NewID()
	st.Create(store.Meta{ID: mine, Image: "x", Created: time.Now(), Status: sandbox.StatusRunning}) // pre-org = default
	st.Create(store.Meta{ID: theirs, Image: "x", Created: time.Now(), Status: sandbox.StatusRunning, Org: "acme"})

	do := func(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	bearer := func(k string) []string { return []string{"Authorization", "Bearer " + k} }

	// Keys see only their org; other orgs' sandboxes look missing.
	if w := do("GET", "/v1/sandboxes", "", bearer("dawnbx_good")...); !strings.Contains(w.Body.String(), mine) || strings.Contains(w.Body.String(), theirs) {
		t.Errorf("default list: %s", w.Body)
	}
	if w := do("GET", "/v1/sandboxes/"+theirs, "", bearer("dawnbx_good")...); w.Code != 404 {
		t.Errorf("cross-org get: %d", w.Code)
	}
	if w := do("DELETE", "/v1/sandboxes/"+mine, "", bearer(acme)...); w.Code != 404 {
		t.Errorf("cross-org kill: %d", w.Code)
	}
	if w := do("GET", "/v1/keys", "", bearer("dawnbx_good")...); w.Code != 403 {
		t.Errorf("key managed keys: %d", w.Code)
	}

	// Dashboard login.
	if w := do("POST", "/v1/login", `{"username":"admin","password":"bad"}`, "X-Dawnbx", "1"); w.Code != 401 {
		t.Errorf("bad login: %d", w.Code)
	}
	if w := do("POST", "/v1/login", `{"username":"admin","password":"pw"}`); w.Code != 403 {
		t.Errorf("login without X-Dawnbx: %d", w.Code)
	}
	w := do("POST", "/v1/login", `{"username":"admin","password":"pw"}`, "X-Dawnbx", "1")
	ck := w.Result().Cookies()
	if w.Code != 200 || len(ck) != 1 || !ck[0].HttpOnly || ck[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("login: %d %v", w.Code, ck)
	}
	cookie := []string{"Cookie", ck[0].Name + "=" + ck[0].Value}
	if w := do("GET", "/v1/sandboxes", "", cookie...); !strings.Contains(w.Body.String(), theirs) {
		t.Errorf("admin should see every org: %s", w.Body)
	}
	if w := do("POST", "/v1/keys", `{"name":"ci"}`, cookie...); w.Code != 403 {
		t.Errorf("cookie POST without X-Dawnbx: %d", w.Code)
	}
	w = do("POST", "/v1/keys", `{"name":"ci","ttl":"720h"}`, append(cookie, "X-Dawnbx", "1")...)
	var made struct {
		Key  string
		Info auth.Key
	}
	json.Unmarshal(w.Body.Bytes(), &made)
	if w.Code != 200 || !strings.HasPrefix(made.Key, "dbx_") || made.Info.Expires == nil {
		t.Fatalf("create key: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/me", "", bearer(made.Key)...); !strings.Contains(w.Body.String(), `"org":"default"`) {
		t.Errorf("new key: %s", w.Body)
	}
	if w := do("DELETE", "/v1/keys/"+made.Info.ID, "", append(cookie, "X-Dawnbx", "1")...); w.Code != 204 {
		t.Errorf("revoke: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/me", "", bearer(made.Key)...); w.Code != 401 {
		t.Errorf("revoked key: %d", w.Code)
	}
	if w := do("GET", "/v1/audit", "", cookie...); !strings.Contains(w.Body.String(), "key.revoke") {
		t.Errorf("audit: %s", w.Body)
	}
	do("POST", "/v1/logout", "", cookie...)
	if w := do("GET", "/v1/me", "", cookie...); w.Code != 401 {
		t.Errorf("after logout: %d", w.Code)
	}
}

func TestUsersOrgsLockout(t *testing.T) {
	m, st := testManager(t)
	db := testDB(t)
	db.EnsureAdmin("admin", "admin-password")
	h := (&Server{M: m, Auth: db}).Handler()
	do := func(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Dawnbx", "1")
		for i := 0; i < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	login := func(user, pw string) []string {
		w := do("POST", "/v1/login", `{"username":"`+user+`","password":"`+pw+`"}`)
		if w.Code != 200 {
			t.Fatalf("login %s: %d %s", user, w.Code, w.Body)
		}
		c := w.Result().Cookies()[0]
		return []string{"Cookie", c.Name + "=" + c.Value}
	}
	admin := login("admin", "admin-password")

	if w := do("POST", "/v1/orgs", `{"id":"Bad Org"}`, admin...); w.Code != 400 {
		t.Errorf("bad org id: %d", w.Code)
	}
	if w := do("POST", "/v1/orgs", `{"id":"acme","name":"Acme"}`, admin...); w.Code != 200 {
		t.Fatalf("org: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/orgs", `{"id":"acme"}`, admin...); w.Code != 409 {
		t.Errorf("dup org: %d", w.Code)
	}
	if w := do("POST", "/v1/users", `{"username":"bob","password":"short","org":"acme"}`, admin...); w.Code != 400 {
		t.Errorf("short password: %d", w.Code)
	}
	if w := do("POST", "/v1/users", `{"username":"bob","password":"bob-password","org":"acme"}`, admin...); w.Code != 200 || !strings.Contains(w.Body.String(), `"role":"member"`) {
		t.Fatalf("user: %d %s", w.Code, w.Body)
	}
	if w := do("POST", "/v1/users", `{"username":"bob","password":"bob-password","org":"acme"}`, admin...); w.Code != 409 {
		t.Errorf("dup user: %d", w.Code)
	}

	// A member sees and manages only their org.
	ours, theirs := store.NewID(), store.NewID()
	st.Create(store.Meta{ID: ours, Image: "x", Created: time.Now(), Status: sandbox.StatusRunning, Org: "acme"})
	st.Create(store.Meta{ID: theirs, Image: "x", Created: time.Now(), Status: sandbox.StatusRunning})
	bob := login("bob", "bob-password")
	if w := do("GET", "/v1/sandboxes", "", bob...); !strings.Contains(w.Body.String(), ours) || strings.Contains(w.Body.String(), theirs) {
		t.Errorf("member list: %s", w.Body)
	}
	if w := do("GET", "/v1/users", "", bob...); w.Code != 403 {
		t.Errorf("member listed users: %d", w.Code)
	}
	if w := do("POST", "/v1/keys", `{"name":"x","org":"default"}`, bob...); w.Code != 403 {
		t.Errorf("member made key in other org: %d", w.Code)
	}
	if w := do("POST", "/v1/keys", `{"name":"bob-ci"}`, bob...); w.Code != 200 || !strings.Contains(w.Body.String(), `"org":"acme"`) {
		t.Errorf("member key: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/keys", "", bob...); strings.Contains(w.Body.String(), "installer") {
		t.Errorf("member sees default org keys: %s", w.Body)
	}
	if w := do("GET", "/v1/keys", "", admin...); !strings.Contains(w.Body.String(), "installer") || !strings.Contains(w.Body.String(), "bob-ci") {
		t.Errorf("admin sees all keys: %s", w.Body)
	}

	// Nodes: admins only; the join token view is audited; a node holding sandboxes stays.
	m.Self = "srv"
	tok := filepath.Join(t.TempDir(), "node-token")
	os.WriteFile(tok, []byte("K10secret\n"), 0o600)
	sandbox.NodeTokenFile = tok
	for _, n := range []string{"srv", "w1"} {
		m.Kube.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}}}, metav1.CreateOptions{})
	}
	st.Create(store.Meta{ID: store.NewID(), Image: "x", Created: time.Now(), Status: sandbox.StatusRunning, Node: "w1"})
	if w := do("GET", "/v1/nodes", "", bob...); w.Code != 403 {
		t.Errorf("member listed nodes: %d", w.Code)
	}
	if w := do("GET", "/v1/nodes/join", "", bob...); w.Code != 403 {
		t.Errorf("member saw join token: %d", w.Code)
	}
	if w := do("GET", "/v1/nodes", "", admin...); !strings.Contains(w.Body.String(), `"name":"w1"`) || !strings.Contains(w.Body.String(), `"sandboxes":1`) {
		t.Errorf("nodes: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/nodes/join", "", admin...); !strings.Contains(w.Body.String(), "--join https://10.0.0.1:6443 K10secret") {
		t.Errorf("join: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/audit", "", admin...); !strings.Contains(w.Body.String(), "node.join-token.view") {
		t.Errorf("join view not audited: %s", w.Body)
	}
	if w := do("DELETE", "/v1/nodes/w1", "", admin...); w.Code != 409 {
		t.Errorf("removed node holding a sandbox: %d", w.Code)
	}
	if w := do("DELETE", "/v1/nodes/srv", "", admin...); w.Code != 400 {
		t.Errorf("removed server node: %d", w.Code)
	}

	// Password change ends sessions; the old password stops working.
	if w := do("POST", "/v1/me/password", `{"old":"nope-nope-nope","new":"bob-password-2"}`, bob...); w.Code != 403 {
		t.Errorf("wrong old password: %d", w.Code)
	}
	if w := do("POST", "/v1/me/password", `{"old":"bob-password","new":"bob-password-2"}`, bob...); w.Code != 204 {
		t.Fatalf("change password: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/me", "", bob...); w.Code != 401 {
		t.Errorf("session survived password change: %d", w.Code)
	}
	bob = login("bob", "bob-password-2")

	// Lockout after 5 misses; the right password is refused while locked.
	for i := 0; i < 5; i++ {
		if w := do("POST", "/v1/login", `{"username":"bob","password":"wrong-wrong"}`); w.Code != 401 {
			t.Fatalf("miss %d: %d", i, w.Code)
		}
	}
	if w := do("POST", "/v1/login", `{"username":"bob","password":"bob-password-2"}`); w.Code != 429 {
		t.Errorf("not locked out: %d", w.Code)
	}
	if w := do("POST", "/v1/login", `{"username":"admin","password":"admin-password"}`); w.Code != 200 {
		t.Errorf("lockout spilled to other user: %d", w.Code)
	}

	if w := do("DELETE", "/v1/users/admin", "", admin...); w.Code != 400 {
		t.Errorf("deleted self: %d", w.Code)
	}
	if w := do("DELETE", "/v1/users/bob", "", admin...); w.Code != 204 {
		t.Errorf("delete: %d", w.Code)
	}
	if w := do("GET", "/v1/me", "", bob...); w.Code != 401 {
		t.Errorf("deleted user's session: %d", w.Code)
	}

	// Env password: a dashboard change survives restart; a new env value wins.
	do("POST", "/v1/me/password", `{"old":"admin-password","new":"changed-in-ui"}`, admin...)
	db.EnsureAdmin("admin", "admin-password")
	if err := db.CheckPassword("admin", "changed-in-ui"); err != nil {
		t.Error("restart reverted the dashboard password")
	}
	db.EnsureAdmin("admin", "new-env-password")
	if err := db.CheckPassword("admin", "new-env-password"); err != nil {
		t.Error("new env password not applied")
	}
}
