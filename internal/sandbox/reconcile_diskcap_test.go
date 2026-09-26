package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"dawnbx/internal/store"
)

// The cap is "more than 5 GB": a sandbox sitting exactly on the mark is the
// user's 5 GB and must keep running. A grace period that has just run out is
// the same kind of boundary — now.Before(grace) is false the instant it
// expires, so the sandbox is stopped like any other over-cap one.
func TestDiskCapExactlyAtLimit(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	atCap := mk(t, m, "sb-atlimit", nil)
	putPod(t, m, atCap)
	graceOver := mk(t, m, "sb-graceend", func(x *store.Meta) { x.GraceUntil = ptr(now) })
	DiskUsage = func(dir string) int64 {
		if strings.Contains(dir, "atlimit") {
			return DiskLimit
		}
		return DiskLimit + 1
	}

	m.Reconcile(ctx, false)

	if meta, _ := m.Store.ReadMeta(atCap.ID); meta.Status != StatusRunning || meta.Reason != "" {
		t.Errorf("a sandbox exactly at the cap was stopped: %+v", meta)
	}
	if pod(kube, atCap.ID) == nil {
		t.Error("a sandbox at the cap lost its pod")
	}
	if meta, _ := m.Store.ReadMeta(graceOver.ID); meta.Status != StatusStopped || meta.Reason != "over_disk_limit" {
		t.Errorf("a grace period that had already ended still saved the sandbox: %+v", meta)
	}
}

// A sandbox on a worker is measured by running du inside it, so the number
// can come back unreadable (the exec dies) or unparseable (anything but a
// count). Neither is evidence of a big workspace, so neither may stop
// anything; a worker that really does report 6 GB must still be stopped.
func TestUnmeasurableSandboxKeptRunning(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	m.Self = "srv"
	broken := mk(t, m, "sb-duerr001", func(x *store.Meta) { x.Node = "w1" })
	junk := mk(t, m, "sb-dujunk01", func(x *store.Meta) { x.Node = "w1" })
	huge := mk(t, m, "sb-duhuge01", func(x *store.Meta) { x.Node = "w1" })
	m.RunExec = func(_ context.Context, id string, _ []string, _ io.Reader, out, _ io.Writer) (int, error) {
		switch id {
		case junk.ID:
			io.WriteString(out, "du: cannot access /workspace\n")
			return 0, nil
		case huge.ID:
			io.WriteString(out, "6291456\t/workspace\n") // 6 GiB in KB
			return 0, nil
		}
		return 0, errors.New("exec stream closed")
	}

	m.Reconcile(ctx, false)

	for _, s := range []store.Meta{broken, junk} {
		meta, err := m.Store.ReadMeta(s.ID)
		if err != nil || meta.Status != StatusRunning || meta.Reason != "" {
			t.Errorf("%s stopped although its usage could not be read: %+v %v", s.ID, meta, err)
		}
		if pod(kube, s.ID) == nil {
			t.Errorf("%s lost its pod although nothing is known about its size", s.ID)
		}
	}
	if meta, _ := m.Store.ReadMeta(huge.ID); meta.Status != StatusStopped || meta.Reason != "over_disk_limit" {
		t.Errorf("a worker-reported 6 GiB was not measured: %+v", meta)
	}
}

// Stopping a sandbox deletes its pod and keeps its files. If the pod delete
// does not go through, the pod is still running and must be treated as such:
// the sandbox is marked stopped either way, but nothing is thrown away.
func TestStopKeepsFilesWhenPodDeleteFails(t *testing.T) {
	m, kube := setup(t)
	ctx := context.Background()
	s := mk(t, m, "sb-delstp1", nil)
	keep := filepath.Join(m.Store.WS(s.ID), "keep.txt")
	if err := os.WriteFile(keep, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	putPod(t, m, s)
	DiskUsage = func(string) int64 { return DiskLimit + 1 }
	kube.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver gone")
	})

	m.Reconcile(ctx, false)

	meta, err := m.Store.ReadMeta(s.ID)
	if err != nil || meta.Status != StatusStopped || meta.Reason != "over_disk_limit" {
		t.Errorf("over-cap sandbox not stopped: %+v %v", meta, err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("files lost while the pod delete was failing: %v", err)
	}
	if pod(kube, s.ID) == nil {
		t.Error("pod treated as deleted when the delete never went through")
	}
}
