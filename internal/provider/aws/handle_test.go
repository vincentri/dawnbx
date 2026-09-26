package aws

import (
	"errors"
	"strings"
	"testing"

	"dawnbx/internal/provider"
)

// The control plane stores the handle and hands it back verbatim, so every field
// has to survive the JSON round trip: a dropped security group means a worker
// that cannot join, and a dropped parameter name means a credential nobody can
// read or delete.
func TestHandleRoundTrip(t *testing.T) {
	want := handle{
		Stack:          "dawnbx-team-example-com-1a2b3c4d",
		Parameter:      "/dawnbx/bootstrap/dawnbx-team-example-com-1a2b3c4d",
		SecurityGroup:  "sg-0123456789abcdef0",
		LaunchTemplate: "lt-0123456789abcdef0",
		PublicIP:       "203.0.113.7",
		URL:            "https://team.example.com",
		Region:         "eu-west-1",
	}
	h, err := toHandle(want)
	if err != nil {
		t.Fatal(err)
	}
	if h.Empty() {
		t.Fatal("toHandle produced an empty handle")
	}
	got, err := fromHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("round trip changed the handle:\n got %+v\nwant %+v", got, want)
	}
}

// A handle this adapter cannot read names nothing it knows. It must be
// ErrNotFound — the answer every route above already handles — and it must not
// panic: a handle is a database column that an older adapter, a restore or a
// careless migration can put anything in.
func TestHandleThatDoesNotRoundTripIsNotFound(t *testing.T) {
	for _, c := range []struct {
		name string
		h    provider.Handle
	}{
		{"empty", provider.Handle{}},
		{"never written", provider.NewHandle(nil)},
		{"not json", provider.NewHandle([]byte("stack=dawnbx"))},
		{"truncated", provider.NewHandle([]byte(`{"stack":"dawnbx-x"`))},
		{"a list", provider.NewHandle([]byte(`["dawnbx-x"]`))},
		{"no stack", provider.NewHandle([]byte(`{"parameter":"/dawnbx/bootstrap/x"}`))},
		{"no parameter", provider.NewHandle([]byte(`{"stack":"dawnbx-x"}`))},
		{"written by another provider", provider.NewHandle([]byte(`{"project":"x","zone":"y"}`))},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := fromHandle(c.h)
			if !errors.Is(err, provider.ErrNotFound) {
				t.Errorf("error = %v, want ErrNotFound", err)
			}
			if got.usable() {
				t.Errorf("handle %+v decoded as usable", got)
			}
		})
	}
}

// Every operation that takes a handle goes through fromHandle, so a handle from
// somewhere else is refused by all of them the same way and none of them reaches
// a cloud API with an empty stack name.
func TestOperationsRefuseAnUnreadableHandle(t *testing.T) {
	f := newFake(t, func(string, []byte) (int, string) { return 200, "{}" })
	a := newAWS(t, f, nil)
	ctx := testContext(t)
	bad := provider.NewHandle([]byte(`{"nope":true}`))

	if _, err := a.Status(ctx, bad); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Status: %v, want ErrNotFound", err)
	}
	if err := a.Destroy(ctx, bad); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Destroy: %v, want ErrNotFound", err)
	}
	if _, err := a.AddNode(ctx, bad, provider.NodeSpec{InstanceType: "t4g.medium", DiskGiB: 30}, provider.Bootstrap{}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("AddNode: %v, want ErrNotFound", err)
	}
	if err := a.RemoveNode(ctx, bad, "i-1"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("RemoveNode: %v, want ErrNotFound", err)
	}
	if err := a.SetBootstrap(ctx, bad, provider.Bootstrap{AdminPassword: "x"}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("SetBootstrap: %v, want ErrNotFound", err)
	}
	// Not one of them may have reached AWS.
	f.notCalled("DescribeStacks")
	f.notCalled("DeleteStack")
	f.notCalled("RunInstances")
	f.notCalled("AmazonSSM.PutParameter")
}

// Two clusters cannot share a domain, so a domain derives the stack name and a
// second create for it is the same stack — which CloudFormation makes idempotent
// for a request that is still in flight. Without a domain there is nothing to
// derive from, and each create is its own cluster.
func TestStackNameFollowsTheDomain(t *testing.T) {
	first, err := stackName("team.example.com")
	if err != nil {
		t.Fatal(err)
	}
	again, err := stackName("team.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("the same domain named two stacks: %q and %q", first, again)
	}
	if first != "dawnbx-team-example-com" {
		t.Errorf("stack name = %q", first)
	}
	one, err := stackName("")
	if err != nil {
		t.Fatal(err)
	}
	two, err := stackName("")
	if err != nil {
		t.Fatal(err)
	}
	if one == two {
		t.Error("two clusters with no domain shared a stack name")
	}
	for _, n := range []string{first, one} {
		if len(n) > 128 || n[0] < 'a' || n[0] > 'z' {
			t.Errorf("%q is not a stack name CloudFormation accepts", n)
		}
	}
	long, err := stackName(strings.Repeat("sub.", 60) + "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(long) > 128 {
		t.Errorf("a long domain produced a %d character name", len(long))
	}
}
