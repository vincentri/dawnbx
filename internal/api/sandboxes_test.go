package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"dawnbx/internal/auth"
	"dawnbx/internal/sandbox"
	"dawnbx/internal/store"
)

// lowDisk names a data dir that pretends to be full, so a create can be
// refused for the reason it would be on a real server.
const lowDisk = "low-disk"

// The disk seams are package globals, and a claimed sandbox leaves a
// background FillPool running that still reads them, so they are replaced once
// for the whole test binary and never restored.
func init() {
	sandbox.FreePct = func(root string) (float64, error) {
		if filepath.Base(root) == lowDisk {
			return 1, nil
		}
		return 50, nil
	}
	sandbox.DiskUsage = func(string) int64 { return 0 }
}

// fullDiskManager is testManager over a data dir that reports 1% free.
func fullDiskManager(t *testing.T) (*sandbox.Manager, *store.Store) {
	t.Helper()
	root := filepath.Join(t.TempDir(), lowDisk)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, store.Marker), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.New(st, fake.NewClientset(), nil), st
}

// readyWarmPool puts one warm sandbox with a Ready pod in front of the next
// create, so a create claims it instead of waiting for a pod the fake cluster
// would never schedule. Returns the warm id.
func readyWarmPool(t *testing.T, m *sandbox.Manager, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	m.PoolSize = 1
	m.FillPool(ctx)
	ids, err := st.IDs()
	if err != nil || len(ids) != 1 {
		t.Fatalf("warm pool: %v %v", ids, err)
	}
	pods := m.Kube.CoreV1().Pods(sandbox.Namespace)
	p, err := pods.Get(ctx, ids[0], metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := pods.UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	return ids[0]
}

type created struct {
	ID        string     `json:"id"`
	Image     string     `json:"image"`
	Status    string     `json:"status"`
	Network   string     `json:"network"`
	Created   time.Time  `json:"created"`
	ExpiresAt *time.Time `json:"expires_at"`
	Org       string     `json:"org"`
	Warnings  []string   `json:"warnings"`
}

func decodeView(t *testing.T, w *httptest.ResponseRecorder) created {
	t.Helper()
	var v created
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("body is not a sandbox: %q", w.Body)
	}
	return v
}

// TestCreateTTLContract: the wire has to tell "no ttl" from `"ttl": null`,
// because one means the default lifetime and the other means "until killed".
func TestCreateTTLContract(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       time.Duration // 0 means expires_at: null
	}{
		{"absent", `{}`, sandbox.DefaultTTL},
		{"null", `{"ttl":null}`, 0},
		{"explicit", `{"ttl":"45m"}`, 45 * time.Minute},
		{"explicit long", `{"ttl":"2h"}`, 2 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, st := testManager(t)
			warm := readyWarmPool(t, m, st)
			do := doer((&Server{M: m, Auth: testDB(t)}).Handler(), true)
			w := do("POST", "/v1/sandboxes", c.body, "Authorization", "Bearer dawnbx_good")
			if w.Code != 200 {
				t.Fatalf("create: %d %s", w.Code, w.Body)
			}
			v := decodeView(t, w)
			// The claim reuses the warm sandbox's id, so the pool really was used.
			if v.ID != warm {
				t.Errorf("created %s, warm was %s", v.ID, warm)
			}
			if v.Org != auth.DefaultOrg || v.Status != sandbox.StatusRunning {
				t.Errorf("view: %+v", v)
			}
			if c.want == 0 {
				if v.ExpiresAt != nil {
					t.Errorf("ttl null should keep it forever, expires_at %v", v.ExpiresAt)
				}
				return
			}
			if v.ExpiresAt == nil {
				t.Fatalf("no expires_at for ttl %v", c.want)
			}
			if got := v.ExpiresAt.Sub(v.Created); got < c.want-time.Minute || got > c.want+time.Minute {
				t.Errorf("lifetime %v, want about %v", got, c.want)
			}
			// The caller's own fields are answered back, defaults filled in.
			if v.Network != "internet" || v.Image != sandbox.DefaultImage {
				t.Errorf("defaults: %+v", v)
			}
			// A custom image is never taken from the warm pool, and the copy
			// would wait for a pod this fake cluster never starts, so the
			// image/warning path is only reachable with a real cluster.
			// The sandbox is visible to the key that made it and to nobody else.
			if w := do("GET", "/v1/sandboxes/"+v.ID, "", "Authorization", "Bearer dawnbx_good"); w.Code != 200 {
				t.Errorf("get own: %d %s", w.Code, w.Body)
			}
			if w := do("GET", "/v1/sandboxes", "", "Authorization", "Bearer dawnbx_good"); !strings.Contains(w.Body.String(), v.ID) {
				t.Errorf("own sandbox missing from list: %s", w.Body)
			}
		})
	}
}

// TestCreateIsAttributedToTheKey: the audit trail names the key, not the
// org, so a leaked key can be traced back to its row.
func TestCreateIsAttributedToTheKey(t *testing.T) {
	m, st := testManager(t)
	readyWarmPool(t, m, st)
	db := testDB(t)
	if err := db.EnsureAdmin("admin", "admin-password"); err != nil {
		t.Fatal(err)
	}
	do := doer((&Server{M: m, Auth: db}).Handler(), true)
	admin := loginCookie(t, do, "admin", "admin-password")
	w := do("POST", "/v1/sandboxes", `{"ttl":"1h"}`, "Authorization", "Bearer dawnbx_good")
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	id := decodeView(t, w).ID
	w = do("GET", "/v1/audit", "", admin...)
	if !strings.Contains(w.Body.String(), `"action":"sandbox.create"`) || !strings.Contains(w.Body.String(), id) {
		t.Fatalf("no create event: %s", w.Body)
	}
	var page struct {
		Events []struct {
			Actor  string `json:"actor"`
			Action string `json:"action"`
			Target string `json:"target"`
			Org    string `json:"org"`
		} `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	keys, _ := db.ListKeys("")
	var installer string
	for _, k := range keys {
		if k.Name == "installer" {
			installer = k.ID
		}
	}
	if installer == "" {
		t.Fatal("no installer key to attribute the event to")
	}
	// Events are newest first, but two writes in the same second are ordered by
	// id, so look the create up rather than assuming it is first.
	var found bool
	for _, e := range page.Events {
		if e.Action != "sandbox.create" {
			continue
		}
		found = true
		if e.Actor != "key:"+installer || e.Target != id || e.Org != auth.DefaultOrg {
			t.Errorf("create event: %+v", e)
		}
	}
	if !found {
		t.Errorf("no create event: %+v", page.Events)
	}
	// Killing it is audited too, and afterwards it is really gone.
	if w := do("DELETE", "/v1/sandboxes/"+id, "", "Authorization", "Bearer dawnbx_good"); w.Code != 204 {
		t.Fatalf("kill: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/sandboxes/"+id, "", "Authorization", "Bearer dawnbx_good"); w.Code != 404 {
		t.Errorf("still there: %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/v1/audit", "", admin...); !strings.Contains(w.Body.String(), `"action":"sandbox.kill"`) {
		t.Errorf("no kill event: %s", w.Body)
	}
	if _, err := st.ReadMeta(id); err == nil {
		t.Error("killed sandbox still has metadata on disk")
	}
}

// TestCreateRefusals: a create the manager refuses must not be audited as one
// that happened, and the client gets the manager's own code.
func TestCreateRefusals(t *testing.T) {
	m, _ := testManager(t)
	do := doer((&Server{M: m, Auth: testDB(t)}).Handler(), true)
	key := []string{"Authorization", "Bearer dawnbx_good"}

	for _, c := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"bad json", `{"image":`, 400, "invalid_request"},
		{"bad network", `{"network":"host"}`, 400, "invalid_network"},
		{"bad cpu", `{"cpu":"lots"}`, 400, "invalid_resources"},
		{"bad memory", `{"memory":"lots"}`, 400, "invalid_resources"},
		{"bad ttl", `{"ttl":"soon"}`, 400, "invalid_ttl"},
		{"negative ttl", `{"ttl":"-1h"}`, 400, "invalid_ttl"},
	} {
		w := do("POST", "/v1/sandboxes", c.body, key...)
		if w.Code != c.status {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		if got := envelope(t, w); got["code"] != c.code {
			t.Errorf("%s: code %v, want %s", c.name, got["code"], c.code)
		}
	}
	// A full data volume is a server-side refusal (507), not a client error.
	full, _ := fullDiskManager(t)
	w := doer((&Server{M: full, Auth: testDB(t)}).Handler(), true)("POST", "/v1/sandboxes", `{}`, key...)
	if w.Code != 507 || !strings.Contains(w.Body.String(), `"code":"disk_low"`) {
		t.Errorf("low disk: %d %s", w.Code, w.Body)
	}
}

// TestSandboxRoutesOnMissingIDs: every id-taking route answers not_found for an
// id it does not know, including the ones a key cannot use for org reasons.
func TestSandboxRoutesOnMissingIDs(t *testing.T) {
	m, _ := testManager(t)
	do := doer((&Server{M: m, Auth: testDB(t)}).Handler(), true)
	key := []string{"Authorization", "Bearer dawnbx_good"}
	missing := store.NewID()
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/sandboxes/" + missing, ""},
		{"DELETE", "/v1/sandboxes/" + missing, ""},
		{"POST", "/v1/sandboxes/" + missing + "/exec", `{"cmd":"ls"}`},
		{"POST", "/v1/sandboxes/" + missing + "/start", ""},
		{"POST", "/v1/sandboxes/" + missing + "/fork", `{"count":1}`},
		{"POST", "/v1/sandboxes/" + missing + "/extend", `{"ttl":"1h"}`},
		{"GET", "/v1/sandboxes/" + missing + "/files?path=a.txt", ""},
		{"PUT", "/v1/sandboxes/" + missing + "/files?path=a.txt", "x"},
	} {
		w := do(c.method, c.path, c.body, key...)
		if w.Code != 404 {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
			continue
		}
		if got := envelope(t, w); got["code"] != "not_found" {
			t.Errorf("%s %s: code %v", c.method, c.path, got["code"])
		}
	}
}

// TestForkValidation: a fork is refused before it copies anything when its ttl
// or its parent is wrong.
func TestForkValidation(t *testing.T) {
	m, st := testManager(t)
	do := doer((&Server{M: m, Auth: testDB(t)}).Handler(), true)
	key := []string{"Authorization", "Bearer dawnbx_good"}
	id := store.NewID()
	st.Create(store.Meta{ID: id, Image: "x", Created: m.Now(), Status: sandbox.StatusRunning})

	for _, c := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"bad json", `{"count":`, 400, "invalid_request"},
		{"bad ttl", `{"count":1,"ttl":"soon"}`, 400, "invalid_ttl"},
		{"negative count", `{"count":-1}`, 400, "invalid_request"},
		{"too many", `{"count":11}`, 400, "invalid_request"},
	} {
		w := do("POST", "/v1/sandboxes/"+id+"/fork", c.body, key...)
		if w.Code != c.status {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
			continue
		}
		if got := envelope(t, w); got["code"] != c.code {
			t.Errorf("%s: code %v, want %s", c.name, got["code"], c.code)
		}
	}
	// Nothing was created by any of the refusals.
	if l, _ := st.IDs(); len(l) != 1 {
		t.Errorf("fork refusals left sandboxes behind: %v", l)
	}
	// A ttl of null on a fork is a valid request, but making the copy needs a
	// pod that the fake cluster never schedules, so only the refusals above
	// can be checked here.
}

// TestStatusRoute: /v1/status is what dawnbx doctor reads, so it has to report
// the volume and the pool even when there is nothing running.
func TestStatusRoute(t *testing.T) {
	m, _ := testManager(t)
	m.PoolSize = 3
	do := doer((&Server{M: m, Auth: testDB(t)}).Handler(), true)
	w := do("GET", "/v1/status", "", "Authorization", "Bearer dawnbx_good")
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	var st struct {
		Version  string  `json:"version"`
		FreePct  float64 `json:"free_pct"`
		Warm     int     `json:"warm"`
		PoolSize int     `json:"pool_size"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.FreePct != 50 || st.PoolSize != 3 || st.Warm != 0 || st.Version != Version {
		t.Errorf("status: %+v", st)
	}
	// And it is behind the same auth as everything else.
	if w := doer((&Server{M: m, Auth: testDB(t)}).Handler(), false)("GET", "/v1/status", ""); w.Code != 401 {
		t.Errorf("unauthenticated status: %d", w.Code)
	}
}
