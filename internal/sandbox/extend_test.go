package sandbox

import (
	"context"
	"testing"
)

// TestExtendToAnAlreadyPastTTLStillSucceeds: extending a sandbox to a ttl of
// "1ns" is a valid request — parseTTL accepts any positive duration — and the
// instant it names is already in the past by the time it is written.
//
// The previous version wrote the new expiry and then re-read the sandbox through
// Get, and that re-read rejected what the write had just repaired: the caller
// got 410 for an operation that had worked. Start and Create both build their
// view from the state they hold; Extend is the one that re-read, and the re-read
// is what made it wrong.
func TestExtendToAnAlreadyPastTTLStillSucceeds(t *testing.T) {
	m, kube := setup(t)
	markReady(t, kube)
	ctx := context.Background()

	v, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	past := "1ns"
	got, err := m.Extend(ctx, v.ID, &past)
	if err != nil {
		t.Fatalf("extending to a ttl of %q returned %v; the caller asked for exactly the operation that sets the expiry", past, err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("the extended sandbox reports no expiry, so the write did not happen")
	}
	// The stored value must be the one that was written, not a re-read of
	// something else.
	after, err := m.Store.ReadMeta(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExpiresAt == nil || !after.ExpiresAt.Equal(*got.ExpiresAt) {
		t.Errorf("stored expiry %v does not match the returned view's %v", after.ExpiresAt, got.ExpiresAt)
	}
}

// TestExtendRejectsATTLThatIsNotADuration: the fix must not have turned every
// ttl into a success. An unparseable or non-positive ttl is still refused.
func TestExtendRejectsATTLThatIsNotADuration(t *testing.T) {
	m, kube := setup(t)
	markReady(t, kube)
	ctx := context.Background()
	v, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, bad := range []string{"not-a-duration", "0s", "-5m"} {
		if _, err := m.Extend(ctx, v.ID, &bad); err == nil {
			t.Errorf("extending to %q succeeded; an unusable ttl must still be refused", bad)
		}
	}
}

// TestExtendToNilKeepsTheSandboxAlive: a null ttl clears the expiry, which is
// the documented way to keep a sandbox until it is killed.
func TestExtendToNilKeepsTheSandboxAlive(t *testing.T) {
	m, kube := setup(t)
	markReady(t, kube)
	ctx := context.Background()

	v, err := m.Create(ctx, CreateReq{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	one := "1h"
	if _, err := m.Extend(ctx, v.ID, &one); err != nil {
		t.Fatalf("Extend to 1h: %v", err)
	}
	cleared, err := m.Extend(ctx, v.ID, nil)
	if err != nil {
		t.Fatalf("Extend to null: %v", err)
	}
	if cleared.ExpiresAt != nil {
		t.Errorf("a null ttl left an expiry of %v; null means keep until killed", cleared.ExpiresAt)
	}
}
