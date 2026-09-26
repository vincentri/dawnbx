package provider

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageImportsNoAdapter is the mechanical form of the provider-neutrality
// constraint. The constitution requires that no provider-specific identifier
// reaches the shared interface, and this is what makes that checkable instead of
// a matter of opinion: if an AWS type ever appears in this package's signatures,
// this test fails and the leak is caught at review time rather than when the
// second provider is written.
func TestPackageImportsNoAdapter(t *testing.T) {
	fset := token.NewFileSet()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(p, "github.com/aws/") {
				t.Errorf("%s imports %s: the neutral package must not know a cloud SDK", name, p)
			}
			if p == "dawnbx/internal/provider/aws" {
				t.Errorf("%s imports the AWS adapter: the interface must not name an implementation", name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test .go file found; the check is not running")
	}
	t.Logf("%d non-test files checked", checked)
}

// fakeProvider is the double every orchestration test in internal/cluster uses.
// It is deliberately here, in the neutral package's test file, so a test
// provider never has to live in an adapter package to be importable.
type fakeProvider struct {
	id     string
	caps   Capabilities
	states []Status // returned in order, last one repeats
	calls  []string
	boot   []Bootstrap
	handle Handle
}

func (f *fakeProvider) ID() string                 { return f.id }
func (f *fakeProvider) Capabilities() Capabilities { return f.caps }
func (f *fakeProvider) Regions(context.Context) ([]string, error) {
	return f.caps.Regions, nil
}
func (f *fakeProvider) HostSizes(context.Context, string) ([]HostSize, error) {
	return []HostSize{{ID: "small", HourlyUSD: 0.02, MonthlyUSD: 14.6}}, nil
}
func (f *fakeProvider) Estimate(_ context.Context, spec ClusterSpec) (*Estimate, error) {
	return &Estimate{
		QuoteID:  "q-" + spec.InstanceType,
		Hourly:   0.05,
		Monthly:  36.5,
		Lines:    []ChargeLine{{Label: "compute", Hourly: 0.05, Monthly: 36.5}},
		Excluded: []string{"data_transfer", "taxes", "provider_discounts"},
	}, nil
}
func (f *fakeProvider) Create(_ context.Context, _ ClusterSpec, boot Bootstrap) (Handle, error) {
	f.calls = append(f.calls, "Create")
	f.boot = append(f.boot, boot)
	return f.handle, nil
}
func (f *fakeProvider) Status(context.Context, Handle) (Status, error) {
	f.calls = append(f.calls, "Status")
	if len(f.states) == 0 {
		return Status{State: Gone}, nil
	}
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return s, nil
}
func (f *fakeProvider) Destroy(context.Context, Handle) error {
	f.calls = append(f.calls, "Destroy")
	return nil
}
func (f *fakeProvider) AddNode(context.Context, Handle, NodeSpec, Bootstrap) (string, error) {
	f.calls = append(f.calls, "AddNode")
	return "i-fake", nil
}
func (f *fakeProvider) NodeAddrs(_ context.Context, _ Handle, nodes []string) (map[string]string, error) {
	f.calls = append(f.calls, "NodeAddrs")
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		out[n] = "10.0.0.1"
	}
	return out, nil
}
func (f *fakeProvider) RemoveNode(_ context.Context, _ Handle, node string) error {
	f.calls = append(f.calls, "RemoveNode:"+node)
	if node == "busy" {
		return ErrNodeBusy
	}
	return nil
}
func (f *fakeProvider) SetBootstrap(_ context.Context, _ Handle, b Bootstrap) error {
	f.calls = append(f.calls, "SetBootstrap")
	f.boot = append(f.boot, b)
	return nil
}

// newFake builds a fake provider for a test to steer. Unexported on purpose:
// this is package provider's own double, and internal/cluster and internal/api
// each have one of their own, so nothing outside this file can reach either.
func newFake(id string, states ...Status) *fakeProvider {
	return &fakeProvider{
		id:     id,
		caps:   Capabilities{Available: true, Delivery: "test", Regions: []string{"test-1"}},
		states: states,
		handle: NewHandle([]byte(`{"fake":true}`)),
	}
}

func TestRegistryListsUnavailableProviders(t *testing.T) {
	aws := newFake("aws")
	gcp := newFake("gcp")
	gcp.caps.Available = false
	r := NewRegistry()
	r.Register(aws)
	r.Register(gcp)

	if got := r.List(); len(got) != 2 || got[0].ID != "aws" || !got[0].Available || got[1].Available {
		t.Fatalf("list: %+v", got)
	}
	// Registration order, not sorted, so the UI is stable.
	if r.List()[0].ID != "aws" {
		t.Error("list is not in registration order")
	}
	// An unavailable provider is refused, not returned empty.
	if _, err := r.Get("gcp"); err == nil {
		t.Error("an unavailable provider was handed out")
	}
	if _, err := r.Get("nope"); err == nil {
		t.Error("an unknown provider was handed out")
	}
	if p, err := r.Get("aws"); err != nil || p.ID() != "aws" {
		t.Errorf("aws: %v %v", p, err)
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering the same id twice did not panic")
		}
	}()
	r := NewRegistry()
	r.Register(newFake("aws"))
	r.Register(newFake("aws"))
}

func TestHandleIsOpaqueAndCopied(t *testing.T) {
	raw := []byte(`{"stack":"s-1"}`)
	h := NewHandle(raw)
	raw[7] = 'X' // the caller mutating its buffer must not reach the handle
	if string(h.Bytes()) != `{"stack":"s-1"}` {
		t.Errorf("handle aliased its input: %s", h.Bytes())
	}
	if (Handle{}).Empty() != true || h.Empty() != false {
		t.Error("Empty is wrong")
	}
}
