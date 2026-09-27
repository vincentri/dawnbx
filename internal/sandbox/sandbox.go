// Package sandbox runs sandboxes as gVisor pods backed by store metadata.
// meta.json is the source of truth (R16); pods are rebuilt from it.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"dawnbx/internal/store"
)

const (
	Namespace      = "dawnbx-sandboxes"
	DefaultImage   = "python:3.12-slim"
	DefaultTTL     = time.Hour
	DefaultTimeout = 10 * time.Minute // exec (D16)
	CreateTimeout  = 60 * time.Second
	DiskLimit      = 5 << 30 // per-sandbox, bytes
	Grace          = 10 * time.Minute

	// Volume headroom, as a percentage free. AdmitFreePct is the floor for
	// scheduling a sandbox at all; ReclaimFreePct is the level under which the
	// reconciler starts deleting expiring sandboxes and stopping a
	// keep-forever one. They were bare literals at seven sites, so changing one
	// and not another would silently change eviction.
	AdmitFreePct   = 15.0
	ReclaimFreePct = 10.0

	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusDeleting = "deleting"
)

// Error is the API error envelope {code, message, hint}.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	// Cause is what errors.Is matches on. It is never sent to a client - the
	// envelope is the whole contract - and it is nil on every error that is
	// its own root, so Unwrap returning nil leaves those unchanged. It exists
	// because a caller sometimes has to tell one failure from another, and
	// matching on the Code string would tie that to a wire value.
	Cause error `json:"-"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Unwrap exposes Cause so errors.Is and errors.As can see through the envelope.
func (e *Error) Unwrap() error { return e.Cause }

func errf(status int, code, hint, format string, a ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, a...), Hint: hint}
}

func notFound(id string) *Error {
	return errf(404, "not_found", "dawnbx ls shows live sandboxes; expired ones are deleted", "sandbox %s not found", id)
}

// kubeErr maps apiserver outages to the retryable cluster_unavailable (CT6).
func kubeErr(err error) error {
	var se apierrors.APIStatus
	if errors.As(err, &se) {
		return err
	}
	return errf(503, "cluster_unavailable", "the node is restarting; retry in a few seconds", "sandbox runtime unreachable: %v", err)
}

type Manager struct {
	Store *store.Store
	// ListIDs is how the manager enumerates sandboxes. It is a field so a
	// failure can be induced: a store that cannot be read must never read as a
	// server with nothing on it, and that cannot be demonstrated while the only
	// way to fail is a real filesystem.
	ListIDs func() ([]string, error)
	Kube    kubernetes.Interface
	Rest    *rest.Config
	RunExec func(ctx context.Context, id string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	RunTTY  func(ctx context.Context, id string, cmd []string, in io.Reader, out io.Writer, sizes remotecommand.TerminalSizeQueue) (int, error)
	Now     func() time.Time

	// PoolSize warm sandboxes are kept running so create/fork skip pod startup (R10).
	PoolSize int
	// Self is this server's k3s node name. Workspaces of sandboxes on other
	// nodes live on those nodes' disks and are reached through pods.
	Self string

	mu     sync.Mutex
	locks  map[string]*sync.Mutex
	fillMu sync.Mutex
	refill sync.WaitGroup                                   // background FillPool runs started by claim
	low    []string                                         // workers under 15% free disk at the last reconcile; under mu
	diskOf func(ctx context.Context, node string) *nodeDisk // workerDisk; replaced in tests
}

// lowDisk lists workers new sandboxes should avoid.
func (m *Manager) lowDisk() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.low
}

func New(s *store.Store, kube kubernetes.Interface, rc *rest.Config) *Manager {
	m := &Manager{Store: s, Kube: kube, Rest: rc, Now: time.Now, locks: map[string]*sync.Mutex{}}
	// The default reads the real store. Every manager built outside a test gets
	// this, so a failure surfaces exactly as the store reports it.
	if s != nil {
		m.ListIDs = s.IDs
	}
	m.RunExec, m.RunTTY = m.kubeExec, m.kubeTTY
	m.diskOf = m.workerDisk
	return m
}

// lock serialises every mutation of one sandbox, reaper included.
func (m *Manager) lock(id string) func() {
	m.mu.Lock()
	l, ok := m.locks[id]
	if !ok {
		l = &sync.Mutex{}
		m.locks[id] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// View is what the API returns for a sandbox.
type View struct {
	ID          string     `json:"id"`
	Image       string     `json:"image"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	Parent      string     `json:"parent,omitempty"`
	Network     string     `json:"network"`
	Created     time.Time  `json:"created"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RestartedAt *time.Time `json:"restarted_at"`
	Org         string     `json:"org"`
	Node        string     `json:"node,omitempty"` // k3s node holding the workspace
	Warnings    []string   `json:"warnings,omitempty"`
}

type CreateReq struct {
	Image   string  `json:"image"`
	TTL     *string `json:"ttl"` // "1h"; JSON null = keep until killed
	Network string  `json:"network"`
	CPU     string  `json:"cpu"`
	Memory  string  `json:"memory"`
	Org     string  `json:"-"` // set by the API from the caller
	KeyID   string  `json:"-"`
	ttlSet  bool
}

// HasTTL records whether the client sent a ttl key at all (absent = default, null = forever).
func (r *CreateReq) HasTTL() { r.ttlSet = true }

func parseTTL(s string) (time.Duration, *Error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errf(400, "invalid_ttl", `use a duration like "30m" or "2h", or null to keep until killed`, "bad ttl %q", s)
	}
	return d, nil
}

func (m *Manager) Create(ctx context.Context, r CreateReq) (*View, error) {
	if r.Image == "" {
		r.Image = DefaultImage
	}
	if r.Network == "" {
		r.Network = "internet"
	}
	if r.Network != "internet" && r.Network != "none" {
		return nil, errf(400, "invalid_network", `use "internet" or "none"`, "bad network %q", r.Network)
	}
	if r.CPU == "" {
		r.CPU = "1"
	}
	if r.Memory == "" {
		r.Memory = "1Gi"
	}
	if _, err := resource.ParseQuantity(r.CPU); err != nil {
		return nil, errf(400, "invalid_resources", `cpu like "1" or "500m"`, "bad cpu %q", r.CPU)
	}
	if _, err := resource.ParseQuantity(r.Memory); err != nil {
		return nil, errf(400, "invalid_resources", `memory like "1Gi" or "512Mi"`, "bad memory %q", r.Memory)
	}
	if err := m.checkHeadroom(AdmitFreePct); err != nil {
		return nil, err
	}
	now := m.Now().UTC()
	meta := store.Meta{Image: r.Image, Created: now, Status: StatusRunning,
		Network: r.Network, CPU: r.CPU, Memory: r.Memory, Org: r.Org, KeyID: r.KeyID}
	ttl := DefaultTTL
	if r.TTL != nil {
		d, e := parseTTL(*r.TTL)
		if e != nil {
			return nil, e
		}
		ttl = d
	}
	if r.TTL != nil || !r.ttlSet {
		exp := now.Add(ttl)
		meta.ExpiresAt = &exp
	}

	var warn []string // the default image's CMD is just a python REPL, so only custom images get the hint
	if r.Image != DefaultImage {
		warn = []string{"image ENTRYPOINT/CMD is not run; use exec(..., background=true) to start a server"}
	}
	if got, p, unlock, ok := m.claim(ctx, meta); ok {
		defer unlock()
		v := view(got, p)
		v.Warnings = warn
		return v, nil
	}

	meta.ID = store.NewID()
	unlock := m.lock(meta.ID)
	defer unlock()
	if err := m.Store.Create(meta); err != nil {
		return nil, err
	}
	pod, err := m.ensurePod(ctx, meta, "")
	if err == nil {
		pod, err = m.waitReady(ctx, meta.ID)
	}
	if err != nil {
		m.Kube.CoreV1().Pods(Namespace).Delete(context.Background(), meta.ID, metav1.DeleteOptions{})
		os.RemoveAll(m.Store.Dir(meta.ID))
		return nil, err
	}
	meta = m.pin(meta, pod)
	v := view(meta, pod)
	v.Warnings = warn
	return v, nil
}

// pin records the node the scheduler picked, so a recreated pod lands on the
// same disk. Caller holds the lock.
func (m *Manager) pin(meta store.Meta, pod *corev1.Pod) store.Meta {
	if meta.Node == "" && pod != nil && pod.Spec.NodeName != "" {
		meta.Node = pod.Spec.NodeName
		if err := m.Store.WriteMeta(meta); err != nil {
			log.Printf("%s: record node: %v", meta.ID, err)
		}
	}
	return meta
}

// local reports whether meta's workspace is on this server's disk.
func (m *Manager) local(meta store.Meta) bool { return meta.Node == "" || meta.Node == m.Self }

func (m *Manager) podSpec(meta store.Meta, restartedAt string) *corev1.Pod {
	no := false
	ann := map[string]string{}
	if meta.ExpiresAt != nil {
		ann["dawnbx/expires-at"] = meta.ExpiresAt.Format(time.RFC3339)
	}
	if restartedAt != "" {
		ann["dawnbx/restarted-at"] = restartedAt
	}
	// On other nodes the kubelet makes the dir; here store.Create already did.
	hostDir := corev1.HostPathDirectoryOrCreate
	var sel map[string]string
	var aff *corev1.Affinity
	if meta.Node != "" {
		sel = map[string]string{"kubernetes.io/hostname": meta.Node}
	} else if low := m.lowDisk(); len(low) > 0 {
		aff = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpNotIn, Values: low}},
			}}},
		}}
	}
	res := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(meta.CPU),
		corev1.ResourceMemory: resource.MustParse(meta.Memory),
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: meta.ID, Namespace: Namespace, Annotations: ann,
			Labels: map[string]string{"dawnbx/id": meta.ID, "dawnbx/network": meta.Network},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName:              ptr("gvisor"),
			NodeSelector:                  sel,
			Affinity:                      aff,
			AutomountServiceAccountToken:  &no,
			EnableServiceLinks:            &no,
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: ptr(int64(1)),
			Containers: []corev1.Container{{
				Name:       "main",
				Image:      meta.Image,
				Command:    []string{"sleep", "infinity"},
				WorkingDir: "/workspace",
				Env:        []corev1.EnvVar{{Name: "HOME", Value: "/workspace"}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits:   res,
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "ws", MountPath: "/workspace"}},
			}},
			Volumes: []corev1.Volume{{Name: "ws", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: m.Store.WS(meta.ID), Type: &hostDir},
			}}},
		},
	}
}

func ptr[T any](v T) *T { return &v }

// ensurePod creates the pod for meta if missing. Used by create, start and re-adopt.
func (m *Manager) ensurePod(ctx context.Context, meta store.Meta, restartedAt string) (*corev1.Pod, error) {
	pods := m.Kube.CoreV1().Pods(Namespace)
	deadline := m.Now().Add(30 * time.Second)
	for {
		p, err := pods.Create(ctx, m.podSpec(meta, restartedAt), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return pods.Get(ctx, meta.ID, metav1.GetOptions{})
		}
		// Right after install the namespace's default ServiceAccount may not exist yet.
		if err != nil && strings.Contains(err.Error(), "serviceaccount") && m.Now().Before(deadline) {
			time.Sleep(time.Second)
			continue
		}
		if err != nil {
			return nil, kubeErr(err)
		}
		return p, nil
	}
}

func (m *Manager) waitReady(ctx context.Context, id string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, CreateTimeout)
	defer cancel()
	for {
		p, err := m.Kube.CoreV1().Pods(Namespace).Get(ctx, id, metav1.GetOptions{})
		if err != nil && ctx.Err() == nil {
			return nil, kubeErr(err)
		}
		if p != nil {
			if podReady(p) {
				return p, nil
			}
			for _, c := range p.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
					if n := p.Spec.NodeSelector["kubernetes.io/hostname"]; n != "" {
						return nil, errf(507, "no_room", "its files are on node "+n+"; check it under Settings > Nodes", "node %s can't run this sandbox: %s", n, c.Message)
					}
					return nil, errf(507, "no_room", "kill unused sandboxes (dawnbx ls), or add a node (Settings > Nodes)", "no node has room for this sandbox: %s", c.Message)
				}
			}
			for _, cs := range p.Status.ContainerStatuses {
				if w := cs.State.Waiting; w != nil {
					switch w.Reason {
					case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
						return nil, errf(400, "image_pull_failed", "check the image name and that the registry is public", "could not pull image %s: %s", p.Spec.Containers[0].Image, w.Message)
					case "CreateContainerError", "RunContainerError", "CrashLoopBackOff":
						return nil, errf(400, "sandbox_start_failed", "the image needs a `sleep` binary on PATH", "sandbox failed to start: %s %s", w.Reason, w.Message)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, errf(504, "create_timeout", "large images take longer on first pull; retry, or pre-pull on the server", "sandbox not ready after %s", CreateTimeout)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func view(meta store.Meta, pod *corev1.Pod) *View {
	v := &View{ID: meta.ID, Image: meta.Image, Status: meta.Status, Reason: meta.Reason, Parent: meta.Parent,
		Network: meta.Network, Created: meta.Created, ExpiresAt: meta.ExpiresAt, Org: OrgOf(meta), Node: meta.Node}
	if v.Status == "" {
		v.Status = StatusRunning
	}
	if pod != nil {
		if s := pod.Annotations["dawnbx/restarted-at"]; s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				v.RestartedAt = &t
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.RestartCount > 0 && cs.State.Running != nil {
				t := cs.State.Running.StartedAt.UTC()
				v.RestartedAt = &t
			}
		}
	}
	return v
}

// live returns meta for a sandbox that must exist and not be expired.
// OrgOf is the org a sandbox belongs to; sandboxes made before orgs belong to "default".
func OrgOf(meta store.Meta) string {
	if meta.Org == "" {
		return "default"
	}
	return meta.Org
}

// Owner returns id's org, or not_found, so the API can hide other orgs' sandboxes.
func (m *Manager) Owner(id string) (string, error) {
	meta, err := m.Store.ReadMeta(id)
	if err != nil || meta.Status == StatusWarm || meta.Status == StatusDeleting {
		return "", notFound(id)
	}
	return OrgOf(meta), nil
}

func (m *Manager) live(id string) (store.Meta, error) {
	if !store.ValidID(id) {
		return store.Meta{}, notFound(id)
	}
	meta, err := m.Store.ReadMeta(id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (meta.Status == StatusDeleting || meta.Status == StatusWarm)) {
		return meta, notFound(id)
	}
	if err != nil {
		return meta, err
	}
	if meta.ExpiresAt != nil && m.Now().After(*meta.ExpiresAt) {
		return meta, errf(410, "expired", "use ttl=null or sb.extend() for long runs",
			"sandbox %s expired (ttl ended %s)", id, meta.ExpiresAt.Format(time.RFC3339))
	}
	return meta, nil
}

func (m *Manager) running(id string) (store.Meta, error) {
	meta, err := m.live(id)
	if err == nil && meta.Status == StatusStopped {
		err = errf(409, "sandbox_stopped", "call sb.start() first", "sandbox %s is stopped (%s)", id, meta.Reason)
	}
	return meta, err
}

func (m *Manager) Get(ctx context.Context, id string) (*View, error) {
	meta, err := m.live(id)
	if err != nil {
		return nil, err
	}
	p, err := m.Kube.CoreV1().Pods(Namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, kubeErr(err)
	}
	return view(meta, p), nil
}

// ponytail: list reads every meta.json per call; switch to an informer cache past ~1k sandboxes.
func (m *Manager) List(ctx context.Context) ([]*View, error) {
	ids, err := m.ListIDs()
	if err != nil {
		// A store that cannot be read is not an empty server, and this is the
		// inventory an operator reads to decide what they have. Propagating
		// turns a locked or corrupt data dir into an error they can act on.
		return nil, err
	}
	pods, err := m.Kube.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, kubeErr(err)
	}
	byID := map[string]*corev1.Pod{}
	for i := range pods.Items {
		byID[pods.Items[i].Name] = &pods.Items[i]
	}
	out := []*View{}
	for _, id := range ids {
		if meta, err := m.live(id); err == nil {
			out = append(out, view(meta, byID[id]))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

func (m *Manager) Kill(ctx context.Context, id string) error {
	if !store.ValidID(id) {
		return notFound(id)
	}
	unlock := m.lock(id)
	defer unlock()
	meta, err := m.Store.ReadMeta(id)
	if errors.Is(err, store.ErrNotFound) {
		return notFound(id)
	}
	if err != nil {
		return err
	}
	return m.remove(ctx, meta)
}

// remove marks deleting first so a crash midway never brings the sandbox back.
// Caller holds the lock.
func (m *Manager) remove(ctx context.Context, meta store.Meta) error {
	meta.Status = StatusDeleting
	if err := m.Store.WriteMeta(meta); err != nil {
		return err
	}
	err := m.Kube.CoreV1().Pods(Namespace).Delete(ctx, meta.ID, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	if err != nil && !apierrors.IsNotFound(err) {
		return kubeErr(err)
	}
	if !m.local(meta) {
		if _, err := m.onNode(ctx, meta.Node, "rm-"+meta.ID, "rm -rf -- /sb/"+meta.ID); err != nil {
			return err
		}
	}
	return os.RemoveAll(m.Store.Dir(meta.ID)) // RemoveAll does not follow symlinks
}

func (m *Manager) Extend(ctx context.Context, id string, ttl *string) (*View, error) {
	unlock := m.lock(id)
	defer unlock()
	meta, err := m.live(id)
	if err != nil {
		return nil, err
	}
	meta.ExpiresAt = nil
	if ttl != nil {
		d, e := parseTTL(*ttl)
		if e != nil {
			return nil, e
		}
		exp := m.Now().UTC().Add(d)
		meta.ExpiresAt = &exp
	}
	if err := m.Store.WriteMeta(meta); err != nil {
		return nil, err
	}
	m.syncAnnotations(ctx, meta) // failure is fixed by the next reconcile tick
	return m.Get(ctx, id)
}

func (m *Manager) Start(ctx context.Context, id string) (*View, error) {
	unlock := m.lock(id)
	defer unlock()
	meta, err := m.live(id)
	if err != nil {
		return nil, err
	}
	if meta.Status == StatusStopped {
		if meta.Reason == "disk_full" {
			if err := m.checkNode(meta); err != nil {
				return nil, err
			}
		}
		if meta.Reason == "over_disk_limit" {
			g := m.Now().UTC().Add(Grace)
			meta.GraceUntil = &g
		}
		meta.Status, meta.Reason = StatusRunning, ""
		if err := m.Store.WriteMeta(meta); err != nil {
			return nil, err
		}
	}
	if _, err := m.ensurePod(ctx, meta, ""); err != nil {
		return nil, err
	}
	p, err := m.waitReady(ctx, id)
	if err != nil {
		return nil, err
	}
	return view(meta, p), nil
}

// stop deletes the pod and keeps files. Caller holds the lock.
func (m *Manager) stop(ctx context.Context, meta store.Meta, reason string) error {
	meta.Status, meta.Reason, meta.GraceUntil = StatusStopped, reason, nil
	if err := m.Store.WriteMeta(meta); err != nil {
		return err
	}
	err := m.Kube.CoreV1().Pods(Namespace).Delete(ctx, meta.ID, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (m *Manager) syncAnnotations(ctx context.Context, meta store.Meta) {
	p, err := m.Kube.CoreV1().Pods(Namespace).Get(ctx, meta.ID, metav1.GetOptions{})
	if err != nil {
		return
	}
	want := ""
	if meta.ExpiresAt != nil {
		want = meta.ExpiresAt.Format(time.RFC3339)
	}
	if p.Annotations["dawnbx/expires-at"] == want {
		return
	}
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	if want == "" {
		delete(p.Annotations, "dawnbx/expires-at")
	} else {
		p.Annotations["dawnbx/expires-at"] = want
	}
	m.Kube.CoreV1().Pods(Namespace).Update(ctx, p, metav1.UpdateOptions{})
}

type ExecReq struct {
	Cmd        string            `json:"cmd"`
	Timeout    *float64          `json:"timeout"` // seconds; null = no limit
	Background bool              `json:"background"`
	Env        map[string]string `json:"env"`
	timeoutSet bool
}

func (r *ExecReq) HasTimeout() { r.timeoutSet = true }

type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	PID      int    `json:"pid,omitempty"`
	Log      string `json:"log,omitempty"`
}

const maxOutput = 10 << 20

// capped keeps the first maxOutput bytes and drops the rest, so a noisy
// sandbox can't exhaust server memory.
type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (m *Manager) Exec(ctx context.Context, id string, r ExecReq) (*ExecResult, error) {
	if _, err := m.running(id); err != nil {
		return nil, err
	}
	if r.Cmd == "" {
		return nil, errf(400, "invalid_request", `pass a shell command, e.g. "ls -la"`, "cmd is empty")
	}
	var envArgs []string
	for k, v := range r.Env {
		envArgs = append(envArgs, k+"="+v)
	}
	sh := append(append([]string{"env"}, envArgs...), "sh", "-c", r.Cmd)
	if r.Background {
		log := fmt.Sprintf("/tmp/dawnbx-bg-%d.log", m.Now().UnixNano())
		cmd := append(append([]string{"env"}, envArgs...), "sh", "-c",
			`nohup sh -c "$0" >"$1" 2>&1 </dev/null & echo $!`, r.Cmd, log)
		var out capped
		code, err := m.RunExec(ctx, id, cmd, nil, &out, io.Discard)
		if err != nil {
			return nil, err
		}
		pid := 0
		fmt.Sscan(out.String(), &pid)
		return &ExecResult{ExitCode: code, PID: pid, Log: log}, nil
	}
	timeout := DefaultTimeout
	if r.timeoutSet && r.Timeout == nil {
		timeout = 0
	} else if r.Timeout != nil {
		if *r.Timeout <= 0 {
			return nil, errf(400, "invalid_request", "timeout is seconds > 0, or null for no limit", "bad timeout %v", *r.Timeout)
		}
		timeout = time.Duration(*r.Timeout * float64(time.Second))
	}
	if timeout > 0 {
		// The command is killed inside the sandbox; cancelling the stream alone would leave it running.
		// ponytail: needs coreutils/busybox `timeout` in the image; an in-sandbox agent can replace it later.
		secs := fmt.Sprintf("%.3f", timeout.Seconds())
		sh = append([]string{"timeout", "-k", "5", secs}, sh...)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout+15*time.Second)
		defer cancel()
	}
	var stdout, stderr capped
	code, err := m.RunExec(ctx, id, sh, nil, &stdout, &stderr)
	if err != nil {
		if ctx.Err() != nil {
			code = 124
		} else {
			return nil, err
		}
	}
	if timeout > 0 && code == 124 {
		return nil, errf(408, "exec_timeout", "pass timeout= (seconds, or null for no limit) or use background=true",
			"command still running after %s, killed", timeout)
	}
	return &ExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// Files go through exec inside the sandbox, never host paths, so symlinks the
// agent plants in /workspace can't point the server at host files (CT3).
func (m *Manager) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	if _, err := m.running(id); err != nil {
		return nil, err
	}
	var out, stderr capped
	code, err := m.RunExec(ctx, id, []string{"cat", "--", path}, nil, &out, &stderr)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errf(404, "file_not_found", "paths are relative to /workspace", "%s: %s", path, strings.TrimSpace(stderr.String()))
	}
	if out.Len() >= maxOutput {
		return nil, errf(413, "file_too_large", "read files over 10 MB in chunks with exec (e.g. split/head)", "%s is larger than 10 MB", path)
	}
	return out.Bytes(), nil
}

func (m *Manager) WriteFile(ctx context.Context, id, path string, body io.Reader) error {
	if _, err := m.running(id); err != nil {
		return err
	}
	var stderr capped
	code, err := m.RunExec(ctx, id, []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat >"$1"`, "sh", path}, body, io.Discard, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "quota") || strings.Contains(msg, "No space") {
			return errf(507, "disk_limit", "delete files in /workspace; each sandbox has a 5 GB cap", "%s: %s", path, msg)
		}
		return errf(400, "write_failed", "paths are relative to /workspace", "%s: %s", path, msg)
	}
	return nil
}
