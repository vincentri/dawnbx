// dawnbx is the command-line client for a dawnbx server.
//
//	dawnbx ls | create | exec | fork | kill | doctor | version
//
// It reads DAWNBX_URL and DAWNBX_API_KEY, falling back to ~/.dawnbx/env,
// which the installer writes.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `usage: dawnbx <command> [args]

  ls|list                   list sandboxes
  create [--image I] [--network internet|none] [--ttl 1h|forever]
                              create a sandbox and print its id
  exec <id> <cmd...>          run a command; exits with its exit code
  fork <id> [n]               copy a sandbox's /workspace into n new ones
  kill|rm <id>...           delete sandboxes and their files
  doctor                      check the server (more checks when run on it)
  version                     client and server versions

Env: DAWNBX_URL (default http://127.0.0.1:8080), DAWNBX_API_KEY; else ~/.dawnbx/env.
`

var version = "dev"

type sandbox struct {
	ID        string     `json:"id"`
	Image     string     `json:"image"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason"`
	Parent    string     `json:"parent"`
	Network   string     `json:"network"`
	Node      string     `json:"node"`
	Created   time.Time  `json:"created"`
	ExpiresAt *time.Time `json:"expires_at"`
	Warnings  []string   `json:"warnings"`
}

type apiErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

func (e *apiErr) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s\nhint: %s", e.Code, e.Message, e.Hint)
	}
	return e.Code + ": " + e.Message
}

type client struct{ url, key string }

func newClient() *client {
	c := &client{url: os.Getenv("DAWNBX_URL"), key: os.Getenv("DAWNBX_API_KEY")}
	home, err := os.UserHomeDir()
	// Under sudo, use the key of the user who ran the installer.
	if u, e := user.Lookup(os.Getenv("SUDO_USER")); e == nil && os.Getenv("SUDO_USER") != "" {
		home, err = u.HomeDir, nil
	}
	if err == nil && (c.url == "" || c.key == "") {
		b, _ := os.ReadFile(filepath.Join(home, ".dawnbx", "env"))
		for _, line := range strings.Split(string(b), "\n") {
			k, v, _ := strings.Cut(strings.TrimPrefix(line, "export "), "=")
			if k == "DAWNBX_URL" && c.url == "" {
				c.url = v
			} else if k == "DAWNBX_API_KEY" && c.key == "" {
				c.key = v
			}
		}
	}
	if c.url == "" {
		c.url = "http://127.0.0.1:8080"
	}
	c.url = strings.TrimRight(c.url, "/")
	return c
}

func (c *client) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.url+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %v\nhint: is dawnbx-server running? (systemctl status dawnbx)", c.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		e := &apiErr{}
		if json.NewDecoder(resp.Body).Decode(e) != nil || e.Code == "" {
			e.Code, e.Message = "http_error", resp.Status
		}
		if e.Code == "unauthorized" && c.key == "" {
			e.Hint = "set DAWNBX_API_KEY or `source ~/.dawnbx/env`; lost it? re-run install.sh with --new-key"
		}
		return e
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func ago(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	code, err := run(newClient(), os.Args[1], os.Args[2:], os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dawnbx:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(c *client, cmd string, args []string, w io.Writer) (int, error) {
	switch cmd {
	case "ls", "list":
		var r struct{ Sandboxes []sandbox }
		if err := c.do("GET", "/v1/sandboxes", nil, &r); err != nil {
			return 1, err
		}
		// The NODE column only shows once sandboxes run on more than one machine.
		nodes := map[string]bool{}
		for _, s := range r.Sandboxes {
			nodes[s.Node] = true
		}
		multi := len(nodes) > 1
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		hdr := "ID\tSTATUS\tIMAGE\tNETWORK\tPARENT\tAGE\tEXPIRES"
		if multi {
			hdr += "\tNODE"
		}
		fmt.Fprintln(tw, hdr)
		for _, s := range r.Sandboxes {
			exp := "never"
			if s.ExpiresAt != nil {
				exp = "in " + ago(time.Until(*s.ExpiresAt))
			}
			status := s.Status
			if s.Reason != "" {
				status += " (" + s.Reason + ")"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s", s.ID, status, s.Image, s.Network, or(s.Parent, "-"), ago(time.Since(s.Created)), exp)
			if multi {
				fmt.Fprint(tw, "\t"+or(s.Node, "-"))
			}
			fmt.Fprintln(tw)
		}
		return 0, tw.Flush()

	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		image := fs.String("image", "", "container image (default python:3.12-slim)")
		network := fs.String("network", "", "internet or none")
		ttl := fs.String("ttl", "", `lifetime like 30m, or "forever"`)
		if err := fs.Parse(args); err != nil { // flag already printed it with usage
			if errors.Is(err, flag.ErrHelp) {
				return 0, nil
			}
			return 2, nil
		}
		body := map[string]any{}
		if *image != "" {
			body["image"] = *image
		}
		if *network != "" {
			body["network"] = *network
		}
		if *ttl == "forever" {
			body["ttl"] = nil
		} else if *ttl != "" {
			body["ttl"] = *ttl
		}
		var s sandbox
		if err := c.do("POST", "/v1/sandboxes", body, &s); err != nil {
			return 1, err
		}
		for _, warn := range s.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", warn)
		}
		fmt.Fprintln(w, s.ID)
		return 0, nil

	case "exec":
		if len(args) < 2 {
			return 2, fmt.Errorf("usage: dawnbx exec <id> <cmd...>")
		}
		var r struct {
			ExitCode int    `json:"exit_code"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
		}
		// Args are joined like ssh does: `dawnbx exec sb-x 'ls | wc -l'` runs in sh -c.
		if err := c.do("POST", "/v1/sandboxes/"+args[0]+"/exec", map[string]any{"cmd": strings.Join(args[1:], " ")}, &r); err != nil {
			return 1, err
		}
		io.WriteString(w, r.Stdout)
		io.WriteString(os.Stderr, r.Stderr)
		return r.ExitCode, nil

	case "fork":
		if len(args) < 1 {
			return 2, fmt.Errorf("usage: dawnbx fork <id> [n]")
		}
		n := 1
		if len(args) > 1 {
			if _, err := fmt.Sscan(args[1], &n); err != nil {
				return 2, fmt.Errorf("n must be a number, got %q", args[1])
			}
		}
		var r struct{ Sandboxes []sandbox }
		if err := c.do("POST", "/v1/sandboxes/"+args[0]+"/fork", map[string]any{"count": n}, &r); err != nil {
			return 1, err
		}
		for _, s := range r.Sandboxes {
			fmt.Fprintln(w, s.ID)
		}
		return 0, nil

	case "kill", "rm":
		if len(args) == 0 {
			return 2, fmt.Errorf("usage: dawnbx kill <id>...")
		}
		code := 0
		for _, id := range args {
			if err := c.do("DELETE", "/v1/sandboxes/"+id, nil, nil); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", id, err)
				code = 1
			}
		}
		return code, nil

	case "doctor":
		return doctor(c, w), nil

	case "version":
		var v struct{ Version string }
		err := c.do("GET", "/v1/version", nil, &v)
		fmt.Fprintf(w, "client %s\nserver %s\n", version, or(v.Version, "unreachable"))
		return 0, err

	case "help", "-h", "--help":
		fmt.Fprint(w, usage)
		return 0, nil
	}
	return 2, fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// doctor prints one line per check and returns 1 if any failed. API checks run
// anywhere; host checks only on the server itself (as root, for journal/quota).
func doctor(c *client, w io.Writer) int {
	bad := 0
	check := func(name string, err error, fix string) {
		if err == nil {
			fmt.Fprintf(w, "ok    %s\n", name)
			return
		}
		bad = 1
		msg, _, _ := strings.Cut(err.Error(), "\n") // the fix line replaces the API hint
		fmt.Fprintf(w, "FAIL  %s: %s\n", name, msg)
		if fix != "" {
			fmt.Fprintf(w, "      fix: %s\n", fix)
		}
	}
	warn := func(name, msg, fix string) {
		fmt.Fprintf(w, "warn  %s: %s\n      fix: %s\n", name, msg, fix)
	}

	var v struct{ Version string }
	err := c.do("GET", "/v1/version", nil, &v)
	check("server reachable at "+c.url, err, "sudo systemctl restart dawnbx; logs: journalctl -u dawnbx -n 50")
	if err == nil {
		var st struct {
			FreePct  float64 `json:"free_pct"`
			Warm     int     `json:"warm"`
			PoolSize int     `json:"pool_size"`
		}
		err = c.do("GET", "/v1/status", nil, &st)
		check("API key accepted", err, "`source ~/.dawnbx/env`, or re-run install.sh with --new-key")
		if err == nil {
			switch {
			case st.FreePct < 15:
				check("data disk", fmt.Errorf("%.0f%% free; new sandboxes are refused below 15%%", st.FreePct), "kill unused sandboxes (dawnbx ls) or grow the data volume")
			default:
				check(fmt.Sprintf("data disk %.0f%% free", st.FreePct), nil, "")
			}
			if st.PoolSize > 0 && st.Warm < st.PoolSize {
				warn("warm pool", fmt.Sprintf("%d/%d ready; creates are slower until it refills (normal for ~30 s after start)", st.Warm, st.PoolSize),
					"if it stays low: sudo journalctl -u dawnbx | grep pool")
			} else {
				check(fmt.Sprintf("warm pool %d/%d", st.Warm, st.PoolSize), nil, "")
			}
		}
	}

	b, err := os.ReadFile("/etc/dawnbx/install.json")
	if err != nil {
		fmt.Fprintln(w, "(not on the server; run `dawnbx doctor` there for host checks)")
		return bad
	}
	var inst struct {
		DataDir string `json:"data_dir"`
	}
	json.Unmarshal(b, &inst)
	sh := func(script string) error {
		out, err := exec.Command("sh", "-c", script).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s", or(strings.TrimSpace(string(out)), err.Error()))
		}
		return nil
	}
	for _, unit := range []string{"k3s", "dawnbx", "dawnbx-firewall"} {
		check(unit+" service active", sh("systemctl is-active --quiet "+unit), "sudo systemctl restart "+unit+"; logs: journalctl -u "+unit+" -n 50")
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(w, "(cluster checks need root: sudo dawnbx doctor)")
		return bad
	}
	check("gVisor (runsc) installed", sh("runsc --version >/dev/null"), "re-run install.sh")
	check("RuntimeClass gvisor", sh("k3s kubectl get runtimeclass gvisor >/dev/null"), "re-run install.sh (restores /var/lib/rancher/k3s/server/manifests/dawnbx.yaml)")
	check("sandbox network policies", sh("k3s kubectl -n dawnbx-sandboxes get networkpolicy sandbox-deny-all sandbox-egress >/dev/null"), "re-run install.sh")
	if inst.DataDir != "" {
		if sh("findmnt -no OPTIONS --target "+inst.DataDir+" | grep -q prjquota") != nil {
			warn("disk quota", "no ext4 project quota on "+inst.DataDir+"; the 5 GB per-sandbox cap is enforced by a 30 s du check, so a sandbox can overshoot briefly",
				"reinstall with --data-device on a blank disk; the installer formats it with project quotas")
		} else {
			check("disk quota (prjquota) on "+inst.DataDir, nil, "")
		}
	}
	return bad
}
