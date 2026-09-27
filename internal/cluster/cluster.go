// Package cluster owns everything the control plane knows about a cluster it
// did not run: the records, the provisioning state machine, the client it uses
// to reach the cluster once it is up, and the workers on it.
//
// It depends on internal/provider for the neutral interface and never on an
// adapter, so the whole package is testable against a fake provider.
package cluster

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	Regions []string
	Sizes   []provider.HostSize
	MinDisk int
}

// Cluster is the record the control plane keeps, and Node is one worker on it.
//
// Both are aliases of the store's row rather than a second struct beside it.
// They were separate types with the same seventeen and seven fields in the
// same order and the same JSON tags, kept in step by hand through two
// field-for-field mappers - which is the arrangement that lets a column be
// added to one and forgotten in the other, silently, with nothing failing.
// An alias cannot drift, and if a field ever needs to exist in only one of
// them, that is the moment to split them back apart on purpose.
//
// Every field is provider neutral. `ProviderState` is the adapter's opaque
// handle and is the only one that holds anything cloud-specific; it carries no
// json tag, so no response can leak it, and nothing outside this package and
// the adapter may interpret it. A node's `ID` is the handle the adapter gave
// us, not a name we invented, so removing a worker needs no translation table.
type Cluster = auth.Cluster

type Node = auth.ClusterNode

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

// Store is the persistence the Registry needs. internal/auth.DB satisfies it,
// and the interface exists so this package is testable without a driver — but
// it does not buy independence from auth, because the row types above are
// auth's. What it buys is one seam to fake instead of a package to stub: the
// test constructs cluster.Cluster and never names auth.
type Store interface {
	CreateCluster(c Cluster) error
	GetCluster(name string) (*Cluster, error)
	ListClusters() ([]Cluster, error)
	DeleteCluster(name string) error
	SetClusterState(name, status, phase, detail string) error
	SetClusterURL(name, url, pin string) error
	SetPin(name, pin string) error
	SetProviderState(name, state string) error
	SaveCredentials(cluster string, adminEnc, apiEnc []byte) error
	Credentials(cluster string) (admin, api []byte, rotated *time.Time, err error)
	RotateCredentials(cluster string, adminEnc, apiEnc []byte) error
	AddNode(n Node) error
	ListNodes(cluster string) ([]Node, error)
	SetNodeStatus(cluster, id, status, detail string, sandboxes int) error
	DeleteNode(cluster, id string) error
	RecordOp(cluster, kind, phase, detail string) error
	Ops(cluster, kind string, limit int) ([]auth.Op, error)
}

// Registry is the control plane's view of its clusters.
type Registry struct {
	db   Store
	seal *Sealer
	now  func() time.Time
}

func NewRegistry(db Store, seal *Sealer) *Registry {
	return &Registry{db: db, seal: seal, now: time.Now}
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
	if !slices.Contains(cat.Regions, req.Region) {
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
	c := Cluster{
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
	return c, nil
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
//
// Every kind is read, not just the create. A cluster that wedges while a worker
// is being added or credentials rotated records that movement under its own
// kind, and reading only the create would report the create's timestamp as its
// last progress — naming a stall that had already been fixed, or hiding a real
// one behind an old timestamp.
func (r *Registry) LastProgress(name string) (time.Time, error) {
	c, err := r.db.GetCluster(name)
	if err != nil {
		return time.Time{}, err
	}
	ops, err := r.db.Ops(name, "", 0)
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
	return append(make([]Cluster, 0, len(rows)), rows...), nil
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
	return append(make([]Node, 0, len(rows)), rows...), nil
}

// PutNode records a worker.
func (r *Registry) PutNode(n Node) error {
	created := n.Created
	if created.IsZero() {
		created = r.now().UTC()
	}
	return r.db.AddNode(Node{
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
