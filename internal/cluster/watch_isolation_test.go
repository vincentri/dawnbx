package cluster

import (
	"context"
	"sync"
	"testing"
	"time"

	"dawnbx/internal/provider"
)

// keyedProvider answers for one cluster slowly and for the rest immediately,
// keyed on the cluster name in the handle rather than a queue, so a test can be
// precise about which cluster is the slow one.
type keyedProvider struct {
	provider.Provider
	slowKey string
	// release is closed to let the slow call return. Until then a Status for
	// slowName waits, which is what a wedged provider looks like.
	release <-chan struct{}
	mu      sync.Mutex
	asked   map[string]int
}

func (f *keyedProvider) ID() string { return "aws" }

func (f *keyedProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Available: true, Delivery: "test", Regions: []string{"eu-west-1"}}
}

func (f *keyedProvider) Regions(context.Context) ([]string, error) { return []string{"eu-west-1"}, nil }

func (f *keyedProvider) HostSizes(context.Context, string) ([]provider.HostSize, error) {
	return nil, nil
}

func (f *keyedProvider) Estimate(context.Context, provider.ClusterSpec) (*provider.Estimate, error) {
	return &provider.Estimate{QuoteID: "q"}, nil
}

// Create names the handle after the cluster, because that is the only thing
// Status gets: a provider that hands every cluster the same handle cannot tell
// them apart, and a test built on that blocks nothing.
func (f *keyedProvider) Create(_ context.Context, s provider.ClusterSpec, _ provider.Bootstrap) (provider.Handle, error) {
	return provider.NewHandle([]byte(`{"cluster":"` + s.InstanceType + `"}`)), nil
}

func (f *keyedProvider) Status(ctx context.Context, h provider.Handle) (provider.Status, error) {
	f.mu.Lock()
	if f.asked == nil {
		f.asked = map[string]int{}
	}
	key := string(h.Bytes())
	f.asked[key]++
	n := f.asked[key]
	f.mu.Unlock()

	// The slow key blocks on every call until the test releases it; every other
	// key answers immediately. Blocking on every call means the test does not
	// depend on which cluster the loop happens to reach first, which the store's
	// ordering does not promise.
	if key == f.slowKey && f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return provider.Status{}, ctx.Err()
		}
		return provider.Status{State: provider.Bootstrapping}, nil
	}
	_ = n
	return provider.Status{State: provider.Ready, URL: "https://x.example"}, nil
}

func (f *keyedProvider) Destroy(context.Context, provider.Handle) error { return nil }

// TestAWedgedClusterIsAbandonedAtItsOwnTimeout is the property that actually
// holds, and the one worth pinning.
//
// The comment above Watch claimed a slow provider "delays its own cluster and
// nothing else". That was never true and the mutex never made it true: the loop
// is one goroutine stepping clusters in turn, so while a call blocks, no other
// cluster is polled. What the code does guarantee is that the wedged call is
// abandoned at StepTimeout and the loop carries on — that is what the per-cluster
// context is for, and it is the part nothing verified.
//
// So the honest fix is not only to delete the mutex but to say what is true: a
// slow cluster delays the others until its own timeout, and never longer.
func TestAWedgedClusterIsAbandonedAtItsOwnTimeout(t *testing.T) {
	p, reg, _, _ := newProv(t)
	release := make(chan struct{})
	defer close(release)

	cat := Catalogue{Regions: []string{"eu-west-1"}, MinDisk: 20,
		Sizes: []provider.HostSize{{ID: "slowpoke"}, {ID: "quickie"}}}

	fp := &keyedProvider{slowKey: `{"cluster":"slowpoke"}`, release: release}
	p.prov = fp

	for _, name := range []string{"slowpoke", "quickie"} {
		req := CreateRequest{Name: name, Provider: "aws",
			Region: "eu-west-1", InstanceType: name, DiskGiB: 30}
		if _, err := p.Begin(context.Background(), req, cat); err != nil {
			t.Fatalf("Begin(%s): %v", name, err)
		}
	}

	// A short step timeout, so the test is quick and the abandonment is the
	// thing under test rather than the wait.
	p.StepTimeout = 150 * time.Millisecond
	p.PollEvery = time.Millisecond
	p.SetClientFactory(func(string) ClusterClient { return &fakeClient{key: "dbx_1_x"} })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Watch(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// The wedged cluster is never released by the test, so the only thing that
	// can move it on is its own timeout. If the others do not progress after
	// that, the loop is stuck for good and nothing recovers it.
	deadline := time.Now().Add(5 * time.Second)
	quickReady := false
	for time.Now().Before(deadline) {
		if c, _ := reg.Get("quickie"); c != nil && c.Status == StatusReady {
			quickReady = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !quickReady {
		t.Fatal("quickie never became ready even after the wedged cluster's timeout; " +
			"one slow cluster can stall the others for good")
	}

	fp.mu.Lock()
	blocked := fp.asked[fp.slowKey]
	fp.mu.Unlock()
	if blocked == 0 {
		t.Fatal("the wedged cluster was never polled, so nothing was ever abandoned and this proves nothing")
	}
}
