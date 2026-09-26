// Package cluster owns everything the control plane knows about a cluster it
// did not run: the records, the provisioning state machine, the client it uses
// to reach the cluster once it is up, and the workers on it.
//
// It depends on internal/provider for the neutral interface and never on an
// adapter, so the whole package is testable against a fake provider.
package cluster

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"dawnbx/internal/auth"
	"dawnbx/internal/provider"
)

// Status values. These are the neutral ones the provider interface and the
// dashboard share; a provider's own state is translated into them and never
// leaks past this package.
const (
	StatusProvisioning = "provisioning"
	StatusReady        = "ready"
	StatusFailed       = "failed"
	StatusDeleting     = "deleting"
)

// Phase values, appended to cluster_ops as the state machine advances. The names
// say what the *product* is doing, not which cloud call is running: the AWS
// adapter does a parameter write and a stack create inside `requesting_host`, and
// the dashboard renders that row without knowing either.
const (
	PhaseValidating     = "validating"
	PhaseRequestingHost = "requesting_host"
	PhaseBootstrapping  = "bootstrapping"
	PhaseVerifying      = "verifying"
	PhaseMintingKey     = "minting_key"
	PhaseReady          = "ready"
	PhaseFailed         = "failed"
)

var (
	// ErrNotFound is a cluster name nothing matches.
	ErrNotFound = errors.New("cluster not found")
	// ErrExists is a name already taken.
	ErrExists = errors.New("cluster already exists")
	// ErrInvalid is a rejected configuration; the message says what was wrong.
	ErrInvalid = errors.New("invalid")
	// ErrCredentialsNotReady means the API key does not exist yet, because the
	// cluster mints it for itself once it accepts the injected password.
	ErrCredentialsNotReady = errors.New("cluster credentials are not ready yet")
)

// nameRe keeps a cluster name a DNS-1123 label, because it becomes a
// CloudFormation stack name, a URL segment, and an SSM parameter suffix.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)

var domainRe = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

// Catalogue is what a provider says it can build, so validation here never
// hard-codes a cloud's instance types. The AWS adapter reads its own template.
type Catalogue struct {
	Regions   []string
	Sizes     []provider.HostSize
	MinDisk   int
	ValidName func(string) bool
}

// Cluster is the record the control plane keeps. Every field is provider
// neutral: `provider_state` is the adapter's opaque handle and is the only
// column that holds anything cloud-specific, and nothing outside this package
// and the adapter may interpret it.
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

// Node is one worker. `ID` is the handle the adapter gave us, not a name we
// invented, so removing a node needs no translation table.
type Node struct {
	Cluster      string    `json:"-"`
	ID           string    `json:"id"`
	InstanceType string    `json:"instance_type"`
	Status       string    `json:"status"`
	Detail       string    `json:"detail"`
	Sandboxes    int       `json:"sandboxes"`
	Created      time.Time `json:"created"`
}

// Op is one row of phase history. The last row for a (cluster, kind) is the
// current phase, and the phase-change times in that history are what a stall is
// measured against. Nothing surfaces the rows themselves yet, so there is no
// route that returns them — the table records what happened, and the first
// reader is the stall check.
type Op struct {
	ID      string    `json:"-"`
	Cluster string    `json:"-"`
	Kind    string    `json:"kind"`
	Phase   string    `json:"phase"`
	Detail  string    `json:"detail"`
	Created time.Time `json:"created"`
}

// CreateRequest is the validated shape a create arrives as.
type CreateRequest struct {
	Name         string
	Provider     string
	Region       string
	InstanceType string
	DiskGiB      int
	Domain       string
	QuoteID      string
}

// Store is the persistence the Registry needs. internal/auth.DB satisfies it;
// the interface exists so this package is testable without SQLite.
type Store interface {
	CreateCluster(c auth.Cluster) error
	GetCluster(name string) (*auth.Cluster, error)
	ListClusters() ([]auth.Cluster, error)
	DeleteCluster(name string) error
	SetClusterState(name, status, phase, detail string) error
	SetClusterURL(name, url, pin string) error
	SetPin(name, pin string) error
	SetProviderState(name, state string) error
	SaveCredentials(cluster string, adminEnc, apiEnc []byte) error
	Credentials(cluster string) (admin, api []byte, rotated *time.Time, err error)
	RotateCredentials(cluster string, adminEnc, apiEnc []byte) error
	AddNode(n auth.ClusterNode) error
	ListNodes(cluster string) ([]auth.ClusterNode, error)
	SetNodeStatus(cluster, id, status, detail string, sandboxes int) error
	DeleteNode(cluster, id string) error
	RecordOp(cluster, kind, phase, detail string) error
	Ops(cluster, kind string, limit int) ([]auth.Op, error)
}

// Registry is the control plane's view of its clusters.
type Registry struct {
	db    Store
	seal  *Sealer
	now   func() time.Time
	admin string
}

// NewRegistry builds a registry over a store. admin is the org the control
// plane's own credentials belong to; clusters are operator-level, not org-level,
// so this is the one place the value is set.
func NewRegistry(db Store, seal *Sealer, admin string) *Registry {
	return &Registry{db: db, seal: seal, now: time.Now, admin: admin}
}

// SetClock replaces the clock, for tests.
func (r *Registry) SetClock(f func() time.Time) { r.now = f }

// Clock is the registry's clock, so a caller measures elapsed time the same way
// the records were written.
func (r *Registry) Clock() time.Time { return r.now() }

// Validate checks a request against the provider's own catalogue, so a bad
// request costs no cloud call at all.
func (r *Registry) Validate(req CreateRequest, cat Catalogue) error {
	if !nameRe.MatchString(req.Name) {
		return fmt.Errorf("%w: name %q must be a dns-1123 label, 3-32 chars, starting with a letter", ErrInvalid, req.Name)
	}
	if _, err := r.db.GetCluster(req.Name); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, req.Name)
	}
	if !contains(cat.Regions, req.Region) {
		return fmt.Errorf("%w: region %q is not one of %s", ErrInvalid, req.Region, strings.Join(cat.Regions, ", "))
	}
	found := false
	for _, s := range cat.Sizes {
		if s.ID == req.InstanceType {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%w: instance type %q is not offered by %s", ErrInvalid, req.InstanceType, req.Provider)
	}
	if req.DiskGiB < cat.MinDisk {
		return fmt.Errorf("%w: disk must be at least %d GiB", ErrInvalid, cat.MinDisk)
	}
	if req.Domain != "" && !domainRe.MatchString(req.Domain) {
		return fmt.Errorf("%w: domain %q may only contain letters, digits, dots and dashes", ErrInvalid, req.Domain)
	}
	return nil
}

// Create records a cluster in provisioning and writes the first op row. It does
// not touch a cloud: the caller drives the state machine.
func (r *Registry) Create(req CreateRequest, est provider.Estimate, handle provider.Handle) (*Cluster, error) {
	now := r.now().UTC()
	c := auth.Cluster{
		Name:          req.Name,
		Provider:      req.Provider,
		Region:        req.Region,
		InstanceType:  req.InstanceType,
		DiskGiB:       req.DiskGiB,
		Domain:        req.Domain,
		Status:        StatusProvisioning,
		Phase:         PhaseValidating,
		QuoteID:       est.QuoteID,
		HourlyUSD:     est.Hourly,
		MonthlyUSD:    est.Monthly,
		ProviderState: string(handle.Bytes()),
		Created:       now,
		Updated:       now,
	}
	if err := r.db.CreateCluster(c); err != nil {
		return nil, err
	}
	r.db.RecordOp(req.Name, OpCreate, PhaseValidating, "")
	return r.Get(req.Name)
}

// Operation kinds. The history is a list of these interleaved with phases, so
// a worker removal two hours after a rotation is legible rather than being
// folded into the create that preceded it.
const (
	OpCreate     = "create"
	OpAddNode    = "add_node"
	OpRemoveNode = "remove_node"
	OpDelete     = "delete"
	OpRotate     = "rotate"
)

// Phase records a state transition and its op row in one step, so the phase a
// caller reads can never disagree with the history.
//
// The op row is written only when the phase actually moves. A boot that takes
// twenty minutes at a five-second poll is one row in the history, not two
// hundred and forty copies of "bootstrapping".
func (r *Registry) Phase(name, status, phase, detail string) error {
	return r.PhaseFor(name, OpCreate, status, phase, detail)
}

// PhaseFor is Phase with the operation named, for the four operations that are
// not the create.
func (r *Registry) PhaseFor(name, kind, status, phase, detail string) error {
	prev, err := r.db.GetCluster(name)
	if err != nil {
		return err
	}
	if err := r.db.SetClusterState(name, status, phase, detail); err != nil {
		return err
	}
	if prev.Phase == phase && prev.Status == status && prev.Detail == detail {
		return nil
	}
	return r.db.RecordOp(name, kind, phase, detail)
}

// SetURL records the cluster URL and its pinned certificate. It is called only
// on the way to ready: a URL published for a cluster that has not proved it can
// serve would break the one promise the dashboard makes about it.
func (r *Registry) SetURL(name, url, pin string) error { return r.db.SetClusterURL(name, url, pin) }

// SetPin records the certificate pin on its own. The pin is needed on every poll,
// long before the cluster is ready, but the URL is not.
func (r *Registry) SetPin(name, pin string) error { return r.db.SetPin(name, pin) }

// SetHandle replaces the provider's opaque state. Nothing outside this package
// and the adapter may read what is inside it.
func (r *Registry) SetHandle(name string, h provider.Handle) error {
	return r.db.SetProviderState(name, string(h.Bytes()))
}

// Handle returns the opaque provider handle. Nothing outside this package and the
// adapter may read what is inside it.
func (r *Registry) Handle(name string) (provider.Handle, error) {
	c, err := r.db.GetCluster(name)
	if err != nil {
		return provider.Handle{}, err
	}
	return provider.NewHandle([]byte(c.ProviderState)), nil
}

// Get returns one cluster.
func (r *Registry) Get(name string) (*Cluster, error) {
	c, err := r.db.GetCluster(name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		// Anything else is the store failing, not the cluster being absent, and
		// the difference is what an operator needs: one says "create it", the
		// other says "the control plane cannot read its own database".
		return nil, fmt.Errorf("reading cluster %s: %w", name, err)
	}
	return fromAuth(c), nil
}

// LastProgress is when the cluster last moved to a new phase, which is the only
// movement worth measuring a stall against. The updated column moves on every
// poll, so it cannot answer that question.
//
// It reads the operation history, and the phase of each row is what matters: a
// row that only changed the detail — the stall note, most of all — is a report
// about a lack of movement and must not count as movement. Counting it made the
// note reset its own clock, so it was visible for one poll in every twenty
// minutes. So the newest row whose phase differs from the cluster's current one
// is the answer, and a run of detail-only rows is skipped.
func (r *Registry) LastProgress(name string) (time.Time, error) {
	c, err := r.db.GetCluster(name)
	if err != nil {
		return time.Time{}, err
	}
	ops, err := r.db.Ops(name, "create", 0)
	if err != nil {
		return time.Time{}, err
	}
	cur := c.Phase
	// ops arrive newest first, so the first row that is not the current phase is
	// the last time the phase was somewhere else.
	for _, o := range ops {
		if o.Phase != cur {
			return o.Created, nil
		}
	}
	// Every row shares the current phase, so the earliest of them is when it was
	// entered. There is no "no progress" before the cluster's first phase.
	if len(ops) > 0 {
		return ops[len(ops)-1].Created, nil
	}
	return time.Time{}, fmt.Errorf("%w: %s has no recorded progress", ErrNotFound, name)
}

// ByURL finds a cluster by the URL it answers on. The provider hands worker
// operations a URL rather than a name, and the client for that URL has to carry
// the cluster's recorded pin or it cannot be trusted.
func (r *Registry) ByURL(url string) (*Cluster, bool) {
	list, err := r.List()
	if err != nil {
		return nil, false
	}
	for i := range list {
		if list[i].URL != "" && list[i].URL == url {
			return &list[i], true
		}
	}
	return nil, false
}

// List returns every cluster, newest first.
func (r *Registry) List() ([]Cluster, error) {
	rows, err := r.db.ListClusters()
	if err != nil {
		return nil, err
	}
	out := make([]Cluster, 0, len(rows))
	for _, c := range rows {
		out = append(out, *fromAuth(&c))
	}
	return out, nil
}

// SaveAdminPassword seals and stores the one credential injected into the host.
// The API key is deliberately absent: the cluster mints it for itself once it
// accepts this password.
func (r *Registry) SaveAdminPassword(name, password string) error {
	return r.db.SaveCredentials(name, r.seal.Seal([]byte(password)), nil)
}

// MintAPIKey stores the key the cluster issued. It is the second half of the
// credential pair and exists only after the cluster is verified.
func (r *Registry) MintAPIKey(name, key string) error {
	admin, _, _, err := r.db.Credentials(name)
	if err != nil {
		return err
	}
	return r.db.SaveCredentials(name, admin, r.seal.Seal([]byte(key)))
}

// Credentials returns both plaintexts. It refuses before the key exists rather
// than handing over half a credential for a cluster that is not up yet.
func (r *Registry) Credentials(name string) (apiKey, adminPassword string, err error) {
	admin, api, _, err := r.db.Credentials(name)
	if err != nil {
		return "", "", err
	}
	if len(api) == 0 {
		return "", "", ErrCredentialsNotReady
	}
	k, err := r.seal.Open(api)
	if err != nil {
		return "", "", err
	}
	pw, err := r.seal.Open(admin)
	if err != nil {
		return "", "", err
	}
	return string(k), string(pw), nil
}

// KeyID is the identifier half of a key the cluster issued, which is all a
// revocation needs: the secret never has to be recovered to name the key.
func KeyID(token string) string { return clientKeyID(token) }

// AdminPassword returns just the injected password, for the re-delivery a
// rotation needs.
func (r *Registry) AdminPassword(name string) (string, error) {
	admin, _, _, err := r.db.Credentials(name)
	if err != nil {
		return "", err
	}
	pw, err := r.seal.Open(admin)
	if err != nil {
		return "", err
	}
	return string(pw), nil
}

// Rotate replaces both credentials and records when.
func (r *Registry) Rotate(name, apiKey, adminPassword string) error {
	return r.db.RotateCredentials(name, r.seal.Seal([]byte(adminPassword)), r.seal.Seal([]byte(apiKey)))
}

// Nodes lists a cluster's workers.
func (r *Registry) Nodes(name string) ([]Node, error) {
	rows, err := r.db.ListNodes(name)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(rows))
	for _, n := range rows {
		out = append(out, fromAuthNode(&n))
	}
	return out, nil
}

// PutNode records a worker.
func (r *Registry) PutNode(n Node) error {
	created := n.Created
	if created.IsZero() {
		created = r.now().UTC()
	}
	return r.db.AddNode(auth.ClusterNode{
		Cluster: n.Cluster, ID: n.ID, InstanceType: n.InstanceType,
		Status: n.Status, Detail: n.Detail, Sandboxes: n.Sandboxes, Created: created,
	})
}

// SetNodeStatus records a worker's status and the last sandbox count the cluster
// reported. The count is a cache; the cluster is always the authority.
func (r *Registry) SetNodeStatus(cluster, id, status, detail string, sandboxes int) error {
	return r.db.SetNodeStatus(cluster, id, status, detail, sandboxes)
}

// DropNode forgets a worker.
func (r *Registry) DropNode(cluster, id string) error { return r.db.DeleteNode(cluster, id) }

// Delete marks a cluster for deletion, recording it as a delete so the history
// shows the teardown rather than reading as the create finally finishing.
func (r *Registry) Delete(name string) error {
	if err := r.db.SetClusterState(name, StatusDeleting, "", ""); err != nil {
		return err
	}
	return r.db.RecordOp(name, OpDelete, StatusDeleting, "")
}

// Forget removes the record entirely, after the provider has confirmed the
// resources are gone.
func (r *Registry) Forget(name string) error { return r.db.DeleteCluster(name) }

func fromAuth(c *auth.Cluster) *Cluster {
	return &Cluster{
		Name: c.Name, Provider: c.Provider, Region: c.Region, InstanceType: c.InstanceType,
		DiskGiB: c.DiskGiB, Domain: c.Domain, Status: c.Status, Phase: c.Phase,
		Detail: c.Detail, QuoteID: c.QuoteID, HourlyUSD: c.HourlyUSD, MonthlyUSD: c.MonthlyUSD,
		ProviderState: c.ProviderState, URL: c.URL, TLSPin: c.TLSPin,
		Created: c.Created, Updated: c.Updated,
	}
}

func fromAuthNode(n *auth.ClusterNode) Node {
	return Node{Cluster: n.Cluster, ID: n.ID, InstanceType: n.InstanceType, Status: n.Status,
		Detail: n.Detail, Sandboxes: n.Sandboxes, Created: n.Created}
}

// randomPassword mints the one credential that is injected into a new cluster.
// The alphabet is the same shape install.sh uses (base64 with the awkward
// characters stripped), so a password minted here is one the existing installer
// path and the dashboard handle without surprises.
func randomPassword() (string, error) {
	const alpha = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alpha[int(b[i])%len(alpha)]
	}
	return string(b), nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
