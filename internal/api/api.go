// Package api serves the /v1 HTTP API over a sandbox.Manager.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/gorilla/websocket"

	"dawnbx/internal/auth"
	"dawnbx/internal/sandbox"
)

var Version = "dev"

//go:embed ui
var ui embed.FS

type Server struct {
	M    *sandbox.Manager
	Auth *auth.DB
	// Control is the control plane's cluster-management dependency set. It is
	// nil in cluster mode, where the sandbox API is served instead.
	Control *Control
	limit   limiter
}

// ControlPlaneHandler serves the control plane: the identity surface, the
// dashboard, and the provider-neutral cluster routes. It never serves the
// sandbox API, because a control plane has no sandbox runtime — the cluster
// routes that used to answer it say cluster_unavailable instead.
//
// It works with a nil Control. A control plane that started with no provider
// wired in still signs people in and still answers the routes that need no
// cloud, because a server that cannot start is a server nobody can fix the
// credentials from.
func (s *Server) ControlPlaneHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", s.version)
	h := s.router(mux)
	s.sessions(mux)
	s.manage(h)
	s.clusters(h)
	s.noCluster(h)
	s.dashboard(mux)
	return mux
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"version": Version, "api": "v1"})
}

// router wraps fn in the three things every /v1 route shares: authentication,
// the shared error envelope, and 200 for any success.
func (s *Server) router(mux *http.ServeMux) route {
	return func(pattern string, fn func(w http.ResponseWriter, r *http.Request) (any, error)) {
		mux.HandleFunc(pattern, s.auth(func(w http.ResponseWriter, r *http.Request) {
			v, err := fn(w, r)
			if err != nil {
				writeErr(w, err)
				return
			}
			if v != nil {
				writeJSON(w, 200, v)
			}
		}))
	}
}

// sessions is the dashboard's own credential: sign in, sign out. It is not
// behind s.auth, because it is what produces the identity s.auth checks.
func (s *Server) sessions(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Dawnbx") != "1" { // stops cross-site login forms (login CSRF)
			writeErr(w, &sandbox.Error{Status: 403, Code: "forbidden", Message: "login needs the X-Dawnbx: 1 header"})
			return
		}
		var req struct{ Username, Password string }
		if _, err := decode(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		if err := s.limit.check(req.Username, r); err != nil {
			writeErr(w, err)
			return
		}
		tok, p, err := s.Auth.Login(req.Username, req.Password)
		s.limit.record(req.Username, r, err)
		if errors.Is(err, auth.ErrUnauthorized) {
			writeErr(w, &sandbox.Error{Status: 401, Code: "unauthorized", Message: "wrong username or password",
				Hint: "5 misses lock it for 15 min; an admin can set a new password (admin itself: edit admin.env, restart dawnbx)"})
			return
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", MaxAge: int(auth.SessionTTL.Seconds()),
			HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
		s.Auth.Audit(p.Org, p.Actor(), "login", "")
		writeJSON(w, 200, p)
	})
	mux.HandleFunc("POST /v1/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieName); err == nil {
			s.Auth.Logout(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(204)
	})
}

// dashboard is the embedded UI and the catch-all. A path that is not an API
// route answers with the envelope rather than Go's plain text, so a client
// never has to parse two error formats.
func (s *Server) dashboard(mux *http.ServeMux) {
	files := http.FileServerFS(ui)
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/ui/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // names carry a content hash
		} else if _, err := fs.Stat(ui, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
			r.URL.Path = "/ui/" // client-side routes like /ui/settings
		}
		files.ServeHTTP(w, r)
	}
	mux.HandleFunc("GET /{$}", page)
	mux.HandleFunc("GET /ui/", page)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, &sandbox.Error{Status: 404, Code: "not_found", Message: r.Method + " " + r.URL.Path + " is not an API route",
			Hint: "routes live under /v1; the SDK version may not match this server (GET /v1/version)"})
	})
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", s.version)
	h := s.router(mux)
	h("GET /v1/status", func(w http.ResponseWriter, r *http.Request) (any, error) {
		st, err := s.M.Status()
		if err != nil {
			return nil, err
		}
		return map[string]any{"version": Version, "free_pct": st.FreePct, "warm": st.Warm, "pool_size": st.PoolSize}, nil
	})
	// The same probe a control plane answers with true. A client that has to
	// treat a 404 as "not a control plane" will eventually forget to.
	h("GET /v1/control-plane", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := signedIn(r); err != nil {
			return nil, err
		}
		return map[string]any{"control_plane": false, "providers": []string{}, "version": Version}, nil
	})
	h("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req sandbox.CreateReq
		keys, err := decode(r, &req)
		if err != nil {
			return nil, err
		}
		if keys["ttl"] {
			req.HasTTL()
		}
		req.Org, req.KeyID = who(r).Org, who(r).KeyID
		v, err := s.M.Create(r.Context(), req)
		if err == nil {
			s.audit(r, "sandbox.create", v.ID)
		}
		return v, err
	})
	h("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) (any, error) {
		l, err := s.M.List(r.Context())
		if p := who(r); !p.Admin {
			l = slices.DeleteFunc(l, func(v *sandbox.View) bool { return v.Org != p.Org })
		}
		return map[string]any{"sandboxes": l}, err
	})
	h("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.M.Get(r.Context(), r.PathValue("id"))
	})
	h("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := s.M.Kill(r.Context(), r.PathValue("id")); err != nil {
			return nil, err
		}
		s.audit(r, "sandbox.kill", r.PathValue("id"))
		w.WriteHeader(204)
		return nil, nil
	})
	h("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req sandbox.ExecReq
		keys, err := decode(r, &req)
		if err != nil {
			return nil, err
		}
		if keys["timeout"] {
			req.HasTimeout()
		}
		return s.M.Exec(r.Context(), r.PathValue("id"), req)
	})
	h("POST /v1/sandboxes/{id}/fork", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req sandbox.ForkReq
		keys, err := decode(r, &req)
		if err != nil {
			return nil, err
		}
		if keys["ttl"] {
			req.HasTTL()
		}
		l, err := s.M.Fork(r.Context(), r.PathValue("id"), req)
		for _, v := range l {
			s.audit(r, "sandbox.fork", r.PathValue("id")+" -> "+v.ID)
		}
		return map[string]any{"sandboxes": l}, err
	})
	h("POST /v1/sandboxes/{id}/extend", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var req struct {
			TTL *string `json:"ttl"`
		}
		keys, err := decode(r, &req)
		if err != nil {
			return nil, err
		}
		if !keys["ttl"] {
			return nil, &sandbox.Error{Status: 400, Code: "invalid_request", Message: "ttl is required",
				Hint: `pass {"ttl": "1h"}, or {"ttl": null} to keep until killed`}
		}
		return s.M.Extend(r.Context(), r.PathValue("id"), req.TTL)
	})
	h("POST /v1/sandboxes/{id}/start", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.M.Start(r.Context(), r.PathValue("id"))
	})
	h("GET /v1/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) (any, error) {
		b, err := s.M.ReadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
		if err != nil {
			return nil, err
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
		return nil, nil
	})
	h("PUT /v1/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) (any, error) {
		body := http.MaxBytesReader(w, r.Body, 100<<20)
		if err := s.M.WriteFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"), body); err != nil {
			return nil, err
		}
		w.WriteHeader(204)
		return nil, nil
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/terminal", s.auth(s.terminal))

	s.sessions(mux)
	s.manage(h)
	s.dashboard(mux)
	return mux
}

const cookieName = "dawnbx_session"

type ctxKey struct{}

func who(r *http.Request) *auth.Principal { return r.Context().Value(ctxKey{}).(*auth.Principal) }

// auth accepts an API key (Bearer header, or WebSocket subprotocol
// "bearer.<key>") or the dashboard's session cookie, then hides sandboxes of
// other orgs from non-admins.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// RFC 7235: the scheme is case-insensitive, so "bearer" must work too.
		tok, ok := "", false
		if scheme, rest, cut := strings.Cut(r.Header.Get("Authorization"), " "); cut &&
			strings.EqualFold(scheme, "Bearer") {
			tok, ok = strings.TrimSpace(rest), true
		}
		if !ok {
			for _, p := range websocket.Subprotocols(r) {
				if tok, ok = strings.CutPrefix(p, "bearer."); ok {
					break
				}
			}
		}
		var p *auth.Principal
		var err error
		if ok {
			p, err = s.Auth.CheckKey(tok)
		} else if c, cerr := r.Cookie(cookieName); cerr == nil {
			// SameSite=Strict already stops cross-site posts; a custom header also
			// forces a CORS preflight that same-site sibling hosts can't pass.
			if r.Method != "GET" && r.Header.Get("X-Dawnbx") != "1" {
				writeErr(w, &sandbox.Error{Status: 403, Code: "forbidden", Message: "cookie requests need the X-Dawnbx: 1 header",
					Hint: "API clients should send Authorization: Bearer <key> instead"})
				return
			}
			p, err = s.Auth.CheckSession(c.Value)
		} else {
			err = auth.ErrUnauthorized
		}
		if errors.Is(err, auth.ErrUnauthorized) {
			writeErr(w, &sandbox.Error{Status: 401, Code: "unauthorized", Message: "missing or wrong API key",
				Hint: "set DAWNBX_API_KEY to the key the installer printed, or make one in the dashboard"})
			return
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		if id := r.PathValue("id"); id != "" && !p.Admin {
			org, err := s.M.Owner(id)
			if err == nil && org != p.Org {
				err = &sandbox.Error{Status: 404, Code: "not_found", Message: "sandbox " + id + " not found",
					Hint: "dawnbx ls shows live sandboxes; expired ones are deleted"}
			}
			if err != nil {
				writeErr(w, err)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

func (s *Server) audit(r *http.Request, action, target string) {
	p := who(r)
	if err := s.Auth.Audit(p.Org, p.Actor(), action, target); err != nil {
		log.Printf("audit %s %s: %v", action, target, err)
	}
}

// decode fills v from the JSON body and reports which top-level keys were
// present, so the API can tell `"ttl": null` (keep forever) from no ttl (default).
func decode(r *http.Request, v any) (map[string]bool, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		b = []byte("{}")
	}
	bad := func(err error) error {
		return &sandbox.Error{Status: 400, Code: "invalid_request", Message: "bad JSON body: " + err.Error()}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, bad(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, bad(err)
	}
	keys := map[string]bool{}
	for k := range raw {
		keys[k] = true
	}
	return keys, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var e *sandbox.Error
	if !errors.As(err, &e) {
		log.Printf("internal error: %v", err)
		e = &sandbox.Error{Status: 500, Code: "internal", Message: err.Error(), Hint: "check `journalctl -u dawnbx` on the server"}
	}
	if e.Status == 0 {
		// A constructor that forgot its status must never reach WriteHeader:
		// a zero there is a panic in net/http, and a 500 says "the server is
		// at fault" which is the truth about a code with no status attached.
		// The envelope shape is unchanged, and the caller's error is not
		// mutated underneath it.
		log.Printf("error with no status: %v", e)
		c := *e
		c.Status = 500
		e = &c
	}
	writeJSON(w, e.Status, e)
}
