package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Remote is a client for a provisioned cluster's own API.
//
// This is the load-bearing idea in the whole feature. Rather than teaching the
// control plane about k3s join tokens and node inventories, it talks to the
// cluster the way any client would: log in with the administrator password the
// control plane injected, and ask the cluster itself for its own key and its own
// node list. The k3s-specific knowledge stays where it already lives, in the
// cluster, and every provider gets the same code path because every provider
// gives a running host an HTTP API.
//
// It is deliberately not a proxy. Nothing here forwards a sandbox route; SDKs
// and the CLI connect to the cluster URL directly.
type Remote struct {
	BaseURL string
	// Pin is the sha256 of the leaf's SPKI, hex, no separators. Empty before the
	// first Pin call; after that every request must match it.
	Pin string

	cookie string
}

// remoteTimeout is one API call. The cluster is reachable over the internet and
// usually fast; a slow answer must not hold a poll.
const remoteTimeout = 20 * time.Second

// NewRemote builds a client for url with no pin yet. Call Pin before Login.
func NewRemote(url string) *Remote {
	return &Remote{BaseURL: strings.TrimRight(url, "/")}
}

// PinFromLeaf returns the SPKI sha256 of a DER certificate, hex, no separators.
// SPKI rather than the whole certificate so a renewal under the same key does not
// look like a different host, and a different key does.
func PinFromLeaf(der []byte) (string, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), nil
}

// httpClient returns a client that refuses any certificate whose pin is not the
// one recorded.
//
// InsecureSkipVerify is deliberate and narrow: the cluster serves a self-signed
// certificate whenever Let's Encrypt issuance fails, which is a realistic
// outcome on the default sslip.io hostname, so chain validation would report a
// healthy cluster as broken. What makes this safe is that
// VerifyPeerCertificate always enforces the pin — this is not "trust anything",
// it is "trust this specific key", and the dashboard shows the fingerprint so an
// operator can confirm it out of band.
func (r *Remote) httpClient() *http.Client {
	want := r.Pin
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // enforced by VerifyPeerCertificate below
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server sent no certificate")
			}
			got, err := PinFromLeaf(rawCerts[0])
			if err != nil {
				return err
			}
			if want != "" && got != want {
				return fmt.Errorf("cluster certificate pin mismatch: have %s, want %s", got, want)
			}
			return nil
		},
	}}
	return &http.Client{Timeout: remoteTimeout, Transport: tr}
}

// EstablishPin reads the cluster's certificate, stores the pin, and returns the
// fingerprint for the dashboard to show. This is trust on first use: the pin
// comes from the first connection, so an operator who wants more must compare it
// against the host out of band.
func (r *Remote) EstablishPin(ctx context.Context, url string) (string, error) {
	target := strings.TrimRight(url, "/")
	if target == "" {
		target = r.BaseURL
	}
	// This is the one call made without the pin, because the pin is what it
	// fetches. It reads nothing but the TLS handshake.
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // the pin is established here
	defer tr.CloseIdleConnections()
	c := &http.Client{Timeout: remoteTimeout, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"/v1/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach the cluster at %s: %w", target, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return "", fmt.Errorf("no certificate from %s", target)
	}
	pin, err := PinFromLeaf(resp.TLS.PeerCertificates[0].Raw)
	if err != nil {
		return "", err
	}
	r.BaseURL, r.Pin = target, pin
	return pin, nil
}

// errNoSession is returned before any request that needs the cluster's own
// session. Catching it here turns a confusing 401 from a cluster that is
// several seconds away into an immediate, obvious message.
var errNoSession = errors.New("not signed in to the cluster; call Login first")

// sessioned reports whether this client holds a cluster session.
func (r *Remote) sessioned() error {
	if r.cookie == "" {
		return errNoSession
	}
	return nil
}

func (r *Remote) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The cluster refuses a cookie write without this header, because a
	// session change must be provably same-site (internal/api/api.go).
	req.Header.Set("X-Dawnbx", "1")
	if r.cookie != "" {
		req.Header.Set("Cookie", r.cookie)
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if c := resp.Cookies(); len(c) > 0 {
		for _, ck := range c {
			if ck.Name == "dawnbx_session" && ck.Value != "" {
				r.cookie = ck.Name + "=" + ck.Value
			}
		}
	}
	return resp, nil
}

// Login posts the administrator password and keeps the session cookie the
// cluster issues. Nothing here invents a token.
func (r *Remote) Login(ctx context.Context, password string) error {
	resp, err := r.do(ctx, http.MethodPost, "/v1/login",
		map[string]string{"username": "admin", "password": password})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cluster login failed: %s", resp.Status)
	}
	if r.cookie == "" {
		return fmt.Errorf("cluster login returned no session cookie")
	}
	return nil
}

// MintAPIKey asks the cluster for a key of its own. This is why the key is never
// injected: the cluster issues something it can list, revoke and audit, instead
// of carrying an imported hash its own dashboard cannot see.
func (r *Remote) MintAPIKey(ctx context.Context, name string) (string, error) {
	if err := r.sessioned(); err != nil {
		return "", err
	}
	resp, err := r.do(ctx, http.MethodPost, "/v1/keys", map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cluster refused to mint a key: %s", resp.Status)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("cluster key: %w", err)
	}
	if out.Key == "" {
		return "", fmt.Errorf("cluster returned an empty key")
	}
	return out.Key, nil
}

// KeyID is the identifier part of a key the cluster issued. A key is
// dbx_<id>_<secret>, so the id is the middle segment and naming it to revoke
// does not require holding the secret.
func clientKeyID(token string) string {
	parts := strings.SplitN(token, "_", 3)
	if len(parts) < 3 || parts[0] != "dbx" || parts[1] == "" {
		return ""
	}
	return parts[1]
}

// RevokeKey retires a key the control plane issued earlier. Without this a
// rotation leaves the previous key live on the cluster, which is the opposite of
// what an operator rotating a leaked credential is asking for.
func (r *Remote) RevokeKey(ctx context.Context, keyID string) error {
	if keyID == "" {
		return errors.New("cannot revoke a key with no id")
	}
	if err := r.sessioned(); err != nil {
		return err
	}
	resp, err := r.do(ctx, http.MethodDelete, "/v1/keys/"+keyID, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		// Already gone. A rotation must not fail because the key it is
		// replacing was revoked by hand in the meantime.
		return nil
	default:
		return fmt.Errorf("cluster refused to revoke key %s: %s", keyID, resp.Status)
	}
}

// RemoteNode is what the cluster reports about one of its workers.
type RemoteNode struct {
	Name string `json:"name"`
	// Addr is the address the cluster reaches this node on. It is the only
	// identifier the two sides are guaranteed to agree on: a cluster names
	// its nodes by hostname, while the control plane holds whatever its
	// provider calls the machine, and matching on Name alone left every joined
	// worker reported as provisioning for ever.
	Addr      string `json:"ip"`
	Ready     bool   `json:"ready"`
	Sandboxes int    `json:"sandboxes"`
}

// Nodes asks the cluster which workers it has and how many sandboxes each holds.
// The cluster is the authority; the control plane only caches the answer.
func (r *Remote) Nodes(ctx context.Context) ([]RemoteNode, error) {
	if err := r.sessioned(); err != nil {
		return nil, err
	}
	resp, err := r.do(ctx, http.MethodGet, "/v1/nodes", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cluster nodes: %s", resp.Status)
	}
	var out struct {
		Nodes []RemoteNode `json:"nodes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("cluster nodes: %w", err)
	}
	return out.Nodes, nil
}

// JoinCommand asks the cluster for the command that adds a worker. The cluster
// owns the token format and the address; the control plane never learns either.
func (r *Remote) JoinCommand(ctx context.Context) (string, error) {
	if err := r.sessioned(); err != nil {
		return "", err
	}
	resp, err := r.do(ctx, http.MethodGet, "/v1/nodes/join", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cluster join command: %s", resp.Status)
	}
	var out struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("cluster join command: %w", err)
	}
	if out.Command == "" {
		return "", fmt.Errorf("cluster has no join token yet")
	}
	return out.Command, nil
}

// ErrNodeBusyFromCluster reports that the cluster refused to remove a worker
// because it still holds sandboxes. The control plane relays this; it never
// removes a busy node on the strength of its own cached count.
var ErrNodeBusyFromCluster = fmt.Errorf("node still holds sandboxes")

// RemoveNode asks the cluster to remove a worker. A 409 means the cluster
// disagrees with us, and the cluster wins.
func (r *Remote) RemoveNode(ctx context.Context, node string) error {
	if err := r.sessioned(); err != nil {
		return err
	}
	resp, err := r.do(ctx, http.MethodDelete, "/v1/nodes/"+node, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("%w: %s still holds sandboxes", ErrNodeBusyFromCluster, node)
	default:
		return fmt.Errorf("cluster refused to remove %s: %s", node, resp.Status)
	}
}
