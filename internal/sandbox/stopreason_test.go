package sandbox

import (
	"context"
	"testing"
	"time"

	"dawnbx/internal/store"
)

// The typed constants are the compile-time fix; these pin the behaviour they
// decide, so the strings stop being load-bearing and the outcome stays pinned.
//
// Start treats the two reasons differently on purpose. diskFull re-checks the
// node and refuses if it is still full, because the sandbox was stopped *because*
// the disk filled. overDiskLimit grants a grace period instead, because the
// sandbox was stopped for being over the limit while the disk itself is fine.
// Getting the two the wrong way round either starts a sandbox onto a full node or
// refuses a restart the operator is entitled to.

// TestStartRefusesOnDiskFullWhenTheNodeIsStillFull
func TestStartRefusesOnDiskFullWhenTheNodeIsStillFull(t *testing.T) {
	m, kube := setup(t)
	markReady(t, kube)
	ctx := context.Background()
	v := mk(t, m, "sb-diskfull", func(x *store.Meta) { x.Status, x.Reason = StatusStopped, string(reasonDiskFull) })

	// The node is under the admit threshold, so the check must refuse.
	FreePct = func(string) (float64, error) { return 5, nil }

	if _, err := m.Start(ctx, v.ID); err == nil {
		t.Fatal("Start started a disk_full sandbox onto a node with 5% free")
	} else if e, ok := err.(*Error); !ok || e.Code != "disk_low" {
		t.Errorf("Start returned %v, want the disk_low refusal", err)
	}
}

// TestStartGrantsGraceOnOverDiskLimit
func TestStartGrantsGraceOnOverDiskLimit(t *testing.T) {
	m, kube := setup(t)
	markReady(t, kube)
	ctx := context.Background()
	v := mk(t, m, "sb-overlim", func(x *store.Meta) { x.Status, x.Reason = StatusStopped, string(reasonOverDiskLimit) })

	// The disk is fine; the sandbox was simply over the limit. Start must not
	// re-check headroom, must grant a grace period, and must start it.
	FreePct = func(string) (float64, error) { return 90, nil }

	if _, err := m.Start(ctx, v.ID); err != nil {
		t.Fatalf("Start refused an over_disk_limit sandbox on a node with room: %v", err)
	}
	after, err := m.Store.ReadMeta(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusRunning {
		t.Errorf("status is %q after a successful Start, want %q", after.Status, StatusRunning)
	}
	if after.GraceUntil == nil {
		t.Error("over_disk_limit did not grant a grace period")
	} else if d := time.Until(*after.GraceUntil); d <= 0 || d > Grace+time.Minute {
		t.Errorf("grace period is %v away, want roughly %v", d, Grace)
	}
	if after.Reason != "" {
		t.Errorf("reason is %q after a successful Start, want it cleared", after.Reason)
	}
}

// TestTheStopReasonsAreDistinct: the whole point of the constants is that these
// are different values, and a copy-paste that made them equal would silently
// give every stop the same Start behaviour.
func TestTheStopReasonsAreDistinct(t *testing.T) {
	if reasonDiskFull == reasonOverDiskLimit {
		t.Fatal("the two stop reasons are the same value; Start would treat them identically")
	}
	if string(reasonDiskFull) != "disk_full" || string(reasonOverDiskLimit) != "over_disk_limit" {
		t.Errorf("stored values drifted: %q and %q; the database holds these strings",
			string(reasonDiskFull), string(reasonOverDiskLimit))
	}
}
