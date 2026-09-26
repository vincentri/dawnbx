package sandbox

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"dawnbx/internal/store"
)

// The pod is the contract with k3s: these fields are what keep a tenant out of
// the cluster and pin the workspace to the right disk. Asserted field by field
// on the object the manager actually creates.
func TestPodSpecContract(t *testing.T) {
	m, _ := setup(t)
	exp := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	meta := store.Meta{ID: "sb-spec001", Image: "alpine:3", Network: "none", CPU: "500m",
		Memory: "512Mi", Created: exp, ExpiresAt: &exp}

	p := m.podSpec(meta, "2026-03-01T12:00:05Z")

	if p.Name != "sb-spec001" || p.Namespace != Namespace {
		t.Errorf("identity: %s/%s, want %s/sb-spec001", p.Namespace, p.Name, Namespace)
	}
	if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("runtime class %v: sandboxes must run under gVisor", p.Spec.RuntimeClassName)
	}
	if p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken {
		t.Error("service account token mounted into the sandbox")
	}
	if p.Spec.EnableServiceLinks == nil || *p.Spec.EnableServiceLinks {
		t.Error("service links enabled: the sandbox could reach the API service env vars")
	}
	if p.Spec.RestartPolicy != corev1.RestartPolicyAlways {
		t.Errorf("restart policy %q", p.Spec.RestartPolicy)
	}
	if p.Spec.TerminationGracePeriodSeconds == nil || *p.Spec.TerminationGracePeriodSeconds != 1 {
		t.Errorf("termination grace %v: a killed sandbox should die at once", p.Spec.TerminationGracePeriodSeconds)
	}
	if p.Labels["dawnbx/id"] != "sb-spec001" || p.Labels["dawnbx/network"] != "none" {
		t.Errorf("labels %v", p.Labels)
	}
	// The reaper and the API read expiry off the pod, so the annotation must be
	// the same instant meta.json holds.
	if p.Annotations["dawnbx/expires-at"] != "2026-03-01T12:00:00Z" {
		t.Errorf("expires-at annotation %q", p.Annotations["dawnbx/expires-at"])
	}
	if p.Annotations["dawnbx/restarted-at"] != "2026-03-01T12:00:05Z" {
		t.Errorf("restarted-at annotation %q", p.Annotations["dawnbx/restarted-at"])
	}

	if len(p.Spec.Containers) != 1 {
		t.Fatalf("containers %d", len(p.Spec.Containers))
	}
	c := p.Spec.Containers[0]
	if c.Name != "main" || c.Image != "alpine:3" {
		t.Errorf("container %s = %s", c.Name, c.Image)
	}
	// The image's ENTRYPOINT is never run; the pod must sit idle until exec.
	if len(c.Command) != 2 || c.Command[0] != "sleep" || c.Command[1] != "infinity" {
		t.Errorf("command %v", c.Command)
	}
	if c.WorkingDir != "/workspace" {
		t.Errorf("working dir %q", c.WorkingDir)
	}
	if len(c.Env) != 1 || c.Env[0].Name != "HOME" || c.Env[0].Value != "/workspace" {
		t.Errorf("env %v: HOME must not leak the image's", c.Env)
	}
	// Requests are deliberately tiny so many sandboxes fit; limits are what
	// the user asked for.
	if got := c.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("50m")) != 0 {
		t.Errorf("cpu request %s", got.String())
	}
	if got := c.Resources.Requests[corev1.ResourceMemory]; got.Cmp(resource.MustParse("64Mi")) != 0 {
		t.Errorf("memory request %s", got.String())
	}
	if got := c.Resources.Limits[corev1.ResourceCPU]; got.Cmp(resource.MustParse("500m")) != 0 {
		t.Errorf("cpu limit %s, want the meta value", got.String())
	}
	if got := c.Resources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("512Mi")) != 0 {
		t.Errorf("memory limit %s, want the meta value", got.String())
	}
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/workspace" || c.VolumeMounts[0].Name != "ws" {
		t.Fatalf("volume mounts %v", c.VolumeMounts)
	}
	if len(p.Spec.Volumes) != 1 {
		t.Fatalf("volumes %d", len(p.Spec.Volumes))
	}
	hp := p.Spec.Volumes[0].HostPath
	if hp == nil || hp.Path != m.Store.WS("sb-spec001") {
		t.Errorf("hostPath %v, want the sandbox's own ws/", hp)
	}
	// DirectoryOrCreate: on a worker the dir is not there yet, and the pod must
	// still schedule.
	if hp.Type == nil || *hp.Type != corev1.HostPathDirectoryOrCreate {
		t.Errorf("hostPath type %v", hp.Type)
	}
}

// A workspace that lives on another node's disk must follow it there; one
// with no node yet is free, and a full node is avoided.
func TestPodSpecPlacement(t *testing.T) {
	m, _ := setup(t)
	m.Self = "srv"
	meta := store.Meta{ID: "sb-place01", CPU: "1", Memory: "1Gi", Node: "worker-1"}

	p := m.podSpec(meta, "")
	if p.Spec.NodeSelector["kubernetes.io/hostname"] != "worker-1" {
		t.Errorf("node selector %v, want the pinned node", p.Spec.NodeSelector)
	}
	if p.Spec.Affinity != nil {
		t.Error("affinity set on a pinned sandbox")
	}

	meta.Node = ""
	p = m.podSpec(meta, "")
	if len(p.Spec.NodeSelector) != 0 || p.Spec.Affinity != nil {
		t.Errorf("healthy cluster still constrained: sel=%v aff=%v", p.Spec.NodeSelector, p.Spec.Affinity)
	}

	m.mu.Lock()
	m.low = []string{"worker-1"}
	m.mu.Unlock()
	p = m.podSpec(meta, "")
	req := p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
	if req.Key != "kubernetes.io/hostname" || req.Operator != corev1.NodeSelectorOpNotIn || len(req.Values) != 1 || req.Values[0] != "worker-1" {
		t.Errorf("low-disk avoidance %+v", req)
	}
	// A sandbox pinned to a full node is not moved off it; its files are there.
	p = m.podSpec(store.Meta{ID: "sb-place02", CPU: "1", Memory: "1Gi", Node: "worker-1"}, "")
	if p.Spec.Affinity != nil || p.Spec.NodeSelector["kubernetes.io/hostname"] != "worker-1" {
		t.Errorf("pinned pod not left where its files are: sel=%v aff=%v", p.Spec.NodeSelector, p.Spec.Affinity)
	}
}

// A keep-forever sandbox carries no expiry annotation, and the pod it makes
// carries no restarted-at until one is passed in.
func TestPodSpecNoTTL(t *testing.T) {
	m, _ := setup(t)
	p := m.podSpec(store.Meta{ID: "sb-forever", Image: DefaultImage, CPU: "1", Memory: "1Gi", Network: "internet"}, "")
	if _, ok := p.Annotations["dawnbx/expires-at"]; ok {
		t.Errorf("keep-forever pod has an expiry: %v", p.Annotations)
	}
	if _, ok := p.Annotations["dawnbx/restarted-at"]; ok {
		t.Errorf("fresh pod claims a restart: %v", p.Annotations)
	}
	if p.Labels["dawnbx/network"] != "internet" {
		t.Errorf("network label %q", p.Labels["dawnbx/network"])
	}
}
