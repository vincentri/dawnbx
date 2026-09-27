package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"dawnbx/internal/auth"
	"dawnbx/internal/sandbox"
)

type route func(pattern string, fn func(w http.ResponseWriter, r *http.Request) (any, error))

var (
	orgRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	userRe = regexp.MustCompile(`^[a-zA-Z0-9._@-]{1,64}$`)
)

func bad(msg, hint string) error {
	return &sandbox.Error{Status: 400, Code: "invalid_request", Message: msg, Hint: hint}
}

func forbidden(msg, hint string) error {
	return &sandbox.Error{Status: 403, Code: "forbidden", Message: msg, Hint: hint}
}

// Keys, users and orgs are managed from dashboard sessions only, so a leaked
// API key can't mint more keys. Members manage their own org; admins all orgs.
func signedIn(r *http.Request) error {
	if who(r).User == "" {
		return forbidden("API keys can't manage keys or users", "sign in to the dashboard")
	}
	return nil
}

func adminOnly(r *http.Request) error {
	if err := signedIn(r); err != nil {
		return err
	}
	if !who(r).Admin {
		return forbidden("admins only", "ask an admin; members manage their own org's keys")
	}
	return nil
}

// scope is the org filter for list/revoke: "" (all) for admins.
func scope(r *http.Request) string {
	if who(r).Admin {
		return ""
	}
	return who(r).Org
}

func checkPassword(pw string) error {
	if len(pw) < 10 || len(pw) > 72 { // bcrypt ignores bytes past 72
		return bad("password must be 10 to 72 characters", "")
	}
	return nil
}

func (s *Server) manage(h route) {
	h("GET /v1/me", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return who(r), nil
	})
	h("POST /v1/me/password", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := signedIn(r); err != nil {
			return nil, err
		}
		var req struct {
			Old string `json:"old"`
			New string `json:"new"`
		}
		if _, err := decode(r, &req); err != nil {
			return nil, err
		}
		if err := checkPassword(req.New); err != nil {
			return nil, err
		}
		user := who(r).User
		if err := s.limit.check(user, r); err != nil {
			return nil, err
		}
		err := s.Auth.CheckPassword(user, req.Old)
		s.limit.record(user, r, err)
		if errors.Is(err, auth.ErrUnauthorized) {
			return nil, forbidden("current password is wrong", "")
		}
		if err != nil {
			return nil, err
		}
		if err := s.Auth.SetPassword(user, req.New); err != nil {
			return nil, err
		}
		s.audit(r, "user.password", user)
		w.WriteHeader(204) // every session of the user ended, this one too
		return nil, nil
	})

	h("GET /v1/keys", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := signedIn(r); err != nil {
			return nil, err
		}
		l, err := s.Auth.ListKeys(scope(r))
		return map[string]any{"keys": l}, err
	})
	h("POST /v1/keys", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := signedIn(r); err != nil {
			return nil, err
		}
		var req struct {
			Name string `json:"name"`
			TTL  string `json:"ttl"` // Go duration like "720h"; empty = never expires
			Org  string `json:"org"` // admins only; default = caller's org
		}
		if _, err := decode(r, &req); err != nil {
			return nil, err
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || len(req.Name) > 100 {
			return nil, bad("name is required (up to 100 chars)", `{"name": "ci"}`)
		}
		org := who(r).Org
		if req.Org != "" && req.Org != org {
			if !who(r).Admin {
				return nil, forbidden("members make keys for their own org only", "")
			}
			if ok, err := s.Auth.OrgExists(req.Org); err != nil {
				return nil, err
			} else if !ok {
				return nil, bad("no org "+req.Org, "GET /v1/orgs lists them")
			}
			org = req.Org
		}
		var exp *time.Time
		if req.TTL != "" {
			d, err := time.ParseDuration(req.TTL)
			if err != nil || d <= 0 {
				return nil, bad("bad ttl "+req.TTL, `like "720h" (30 days)`)
			}
			t := time.Now().Add(d).UTC().Truncate(time.Second)
			exp = &t
		}
		tok, k, err := s.Auth.CreateKey(org, req.Name, who(r).Actor(), exp)
		if err != nil {
			return nil, err
		}
		s.audit(r, "key.create", k.ID+" ("+org+")")
		return map[string]any{"key": tok, "info": k}, nil
	})
	h("DELETE /v1/keys/{key}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := signedIn(r); err != nil {
			return nil, err
		}
		ok, err := s.Auth.RevokeKey(scope(r), r.PathValue("key"))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &sandbox.Error{Status: 404, Code: "not_found", Message: "no live key " + r.PathValue("key")}
		}
		s.audit(r, "key.revoke", r.PathValue("key"))
		w.WriteHeader(204)
		return nil, nil
	})
	h("GET /v1/audit", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := signedIn(r); err != nil {
			return nil, err
		}
		l, err := s.Auth.Events(scope(r), 200)
		return map[string]any{"events": l}, err
	})

	h("GET /v1/orgs", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		l, err := s.Auth.ListOrgs()
		return map[string]any{"orgs": l}, err
	})
	h("POST /v1/orgs", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		var req struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if _, err := decode(r, &req); err != nil {
			return nil, err
		}
		if !orgRe.MatchString(req.ID) {
			return nil, bad("org id must be lowercase letters, digits and dashes (up to 40)", `{"id": "acme"}`)
		}
		if req.Name = strings.TrimSpace(req.Name); req.Name == "" {
			req.Name = req.ID
		}
		if err := s.Auth.CreateOrg(req.ID, req.Name); errors.Is(err, auth.ErrExists) {
			return nil, &sandbox.Error{Status: 409, Code: "exists", Message: "org " + req.ID + " already exists"}
		} else if err != nil {
			return nil, err
		}
		s.audit(r, "org.create", req.ID)
		return map[string]string{"id": req.ID, "name": req.Name}, nil
	})

	h("GET /v1/users", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		l, err := s.Auth.ListUsers("")
		return map[string]any{"users": l}, err
	})
	h("POST /v1/users", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Org      string `json:"org"`
			Role     string `json:"role"`
		}
		if _, err := decode(r, &req); err != nil {
			return nil, err
		}
		if !userRe.MatchString(req.Username) {
			return nil, bad("username: letters, digits and . _ @ - (up to 64)", "")
		}
		if err := checkPassword(req.Password); err != nil {
			return nil, err
		}
		if req.Org == "" {
			req.Org = who(r).Org
		}
		if ok, err := s.Auth.OrgExists(req.Org); err != nil {
			return nil, err
		} else if !ok {
			return nil, bad("no org "+req.Org, "POST /v1/orgs makes one")
		}
		if req.Role == "" {
			req.Role = "member"
		}
		if req.Role != "member" && req.Role != "admin" {
			return nil, bad(`role is "member" or "admin"`, "admins see and manage every org")
		}
		u, err := s.Auth.CreateUser(req.Org, req.Username, req.Password, req.Role)
		if errors.Is(err, auth.ErrExists) {
			return nil, &sandbox.Error{Status: 409, Code: "exists", Message: "user " + req.Username + " already exists"}
		}
		if err != nil {
			return nil, err
		}
		s.audit(r, "user.create", u.Username+" ("+u.Org+", "+u.Role+")")
		return u, nil
	})
	h("DELETE /v1/users/{username}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		name := r.PathValue("username")
		if name == who(r).User {
			return nil, bad("you can't delete yourself", "sign in as another admin")
		}
		ok, err := s.Auth.DeleteUser(name)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &sandbox.Error{Status: 404, Code: "not_found", Message: "no user " + name}
		}
		s.audit(r, "user.delete", name)
		w.WriteHeader(204)
		return nil, nil
	})
	h("POST /v1/users/{username}/password", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if err := adminOnly(r); err != nil {
			return nil, err
		}
		var req struct {
			Password string `json:"password"`
		}
		if _, err := decode(r, &req); err != nil {
			return nil, err
		}
		if err := checkPassword(req.Password); err != nil {
			return nil, err
		}
		name := r.PathValue("username")
		if err := s.Auth.SetPassword(name, req.Password); err != nil {
			return nil, &sandbox.Error{Status: 404, Code: "not_found", Message: "no user " + name}
		}
		s.audit(r, "user.password", name)
		w.WriteHeader(204)
		return nil, nil
	})

	// The sandbox runtime's own worker routes. A control plane has no
	// runtime, so it does not register them: the same paths there answer
	// cluster_unavailable, and a handler that dereferenced a nil s.M would
	// panic on the way to saying that.
	if s.M != nil {
		h("GET /v1/nodes", func(w http.ResponseWriter, r *http.Request) (any, error) {
			if err := adminOnly(r); err != nil {
				return nil, err
			}
			l, err := s.M.Nodes(r.Context())
			return map[string]any{"nodes": l}, err
		})
		h("GET /v1/nodes/join", func(w http.ResponseWriter, r *http.Request) (any, error) {
			if err := adminOnly(r); err != nil {
				return nil, err
			}
			cmd, err := s.M.JoinCommand(r.Context())
			if err != nil {
				return nil, err
			}
			s.audit(r, "node.join-token.view", "")
			return map[string]string{"command": cmd}, nil
		})
		h("DELETE /v1/nodes/{name}", func(w http.ResponseWriter, r *http.Request) (any, error) {
			if err := adminOnly(r); err != nil {
				return nil, err
			}
			name := r.PathValue("name")
			if err := s.M.RemoveNode(r.Context(), name); err != nil {
				return nil, err
			}
			s.audit(r, "node.delete", name)
			w.WriteHeader(204)
			return nil, nil
		})
	}
}

// limiter locks a username+IP out for 15 min after 5 wrong passwords, so
// guessing is slow and a stranger can't lock the real admin out from elsewhere.
// ponytail: per API node, in memory; move to the DB if nodes sit behind a round-robin LB.
type limiter struct {
	mu    sync.Mutex
	fails map[string]*fail
}

type fail struct {
	n     int
	until time.Time
}

const (
	maxFails = 5
	lockout  = 15 * time.Minute
)

func limitKey(user string, r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return user + "|" + ip
}

func (l *limiter) check(user string, r *http.Request) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f := l.fails[limitKey(user, r)]; f != nil && time.Now().Before(f.until) {
		return &sandbox.Error{Status: 429, Code: "too_many_attempts", Message: "too many wrong passwords",
			Hint: fmt.Sprintf("try again in %d min", int(time.Until(f.until).Minutes())+1)}
	}
	return nil
}

func (l *limiter) record(user string, r *http.Request, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := limitKey(user, r)
	if !errors.Is(err, auth.ErrUnauthorized) {
		delete(l.fails, k)
		return
	}
	if l.fails == nil || len(l.fails) > 10000 {
		l.fails = map[string]*fail{}
	}
	f := l.fails[k]
	if f == nil || time.Now().After(f.until) && f.n >= maxFails {
		f = &fail{}
		l.fails[k] = f
	}
	if f.n++; f.n >= maxFails {
		f.until = time.Now().Add(lockout)
	}
}
