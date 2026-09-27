package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"dawnbx/internal/store"
)

// readyPodOn is a pod that has landed on a node, which is all pin reads. It does
// not need to be ready: pin records the scheduler's choice, not the pod's health.
func readyPodOn(node string) *corev1.Pod {
	return &corev1.Pod{Spec: corev1.PodSpec{NodeName: node}}
}

// TestCreateFailsWhenTheNodeCannotBeRecorded: pin persists the node the
// scheduler picked so a recreated pod lands on the same disk. The write used to
// be swallowed, so Create reported success with a sandbox whose node had never
// been recorded — the next recreate landed elsewhere, the workspace read as
// empty, and the API caller was told nothing.
//
// The failure is induced by making the sandbox's own directory read-only, so
// WriteMeta's temp file cannot be created. No seam: this is the real filesystem
// failing the way it would with a full or read-only disk, and a test-only hook
// would prove only that the hook was called.
func TestCreateFailsWhenTheNodeCannotBeRecorded(t *testing.T) {
	m, kube := setup(t)
	markReady(t, kube)
	ctx := context.Background()

	// Create once, so there is a sandbox whose directory can be made unwritable.
	v, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	m.withLock(v.ID, func(store.Meta) error { return nil })

	// Clear the recorded node FIRST, while the directory is still writable, so
	// the only thing that fails afterwards is the pin's own write. Doing it the
	// other way round makes this test skip, which is how a test that proves
	// nothing gets merged.
	cur, err := m.Store.ReadMeta(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.Node = ""
	if err := m.Store.WriteMeta(cur); err != nil {
		t.Fatalf("could not clear the node before making the directory read-only: %v", err)
	}

	dir := filepath.Join(m.Store.Root, "sb", v.ID)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skipf("cannot make the directory read-only here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	pod := readyPodOn("node-2")
	_, err = m.pinAndPersist(cur, pod)
	if err == nil {
		t.Fatal("pinAndPersist reported success while the directory could not be written; a node that was never recorded must not read as recorded")
	}
}

// TestPinAndPersistIsANoOpWhenThereIsNothingToRecord: the renamed function must
// keep the two cases where it does not write, or the common path would start
// touching the disk it did not before.
func TestPinAndPersistIsANoOpWhenThereIsNothingToRecord(t *testing.T) {
	m, _ := setup(t)
	already := store.Meta{ID: "sb-alrdy1", Node: "node-1"}

	// Already has a node: no write, no error.
	got, err := m.pinAndPersist(already, readyPodOn("node-2"))
	if err != nil {
		t.Errorf("pinning an already-pinned sandbox: %v", err)
	}
	if got.Node != "node-1" {
		t.Errorf("node became %q; an already-pinned sandbox must not be moved", got.Node)
	}

	// No pod at all: nothing to record.
	got, err = m.pinAndPersist(store.Meta{ID: "sb-nopd1"}, nil)
	if err != nil {
		t.Errorf("pinning with no pod: %v", err)
	}
	if got.Node != "" {
		t.Errorf("node became %q with no pod to read one from", got.Node)
	}
}

// TestPinAndPersistRecordsTheNode: the ordinary path still works, and really
// writes.
func TestPinAndPersistRecordsTheNode(t *testing.T) {
	m, _ := setup(t)
	if err := m.Store.Create(store.Meta{ID: "sb-pinned1", Image: DefaultImage, Network: "internet"}); err != nil {
		t.Fatal(err)
	}
	meta, err := m.Store.ReadMeta("sb-pinned1")
	if err != nil {
		t.Fatal(err)
	}

	got, err := m.pinAndPersist(meta, readyPodOn("node-7"))
	if err != nil {
		t.Fatalf("pinAndPersist: %v", err)
	}
	if got.Node != "node-7" {
		t.Fatalf("node is %q, want node-7", got.Node)
	}
	// And it persisted, rather than only being returned.
	after, err := m.Store.ReadMeta("sb-pinned1")
	if err != nil {
		t.Fatal(err)
	}
	if after.Node != "node-7" {
		t.Errorf("the stored node is %q, want node-7; the write did not happen", after.Node)
	}
}
