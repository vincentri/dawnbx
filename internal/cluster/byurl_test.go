package cluster

import "testing"

// The url lookup is on the path of every sandbox list and every worker add or
// remove, so it used to read every cluster in the control plane and keep one.
// This pins that it asks for one, by counting the store calls the lookup makes
// against a registry holding several clusters.
func TestByURLReadsOneRowRatherThanEveryCluster(t *testing.T) {
	r, m := testRegistry(t)
	for _, name := range []string{"alpha", "beta", "gamma", "delta"} {
		provisioned(t, r, m, name)
	}
	// The one that answers, deliberately not the first.
	if err := r.db.(*memStore).setURL("delta", "https://delta.example"); err != nil {
		t.Fatal(err)
	}

	m.resetCounters()
	c, ok, err := r.ByURL("https://delta.example")
	if err != nil || !ok {
		t.Fatalf("ByURL: %v %v", ok, err)
	}
	if c.Name != "delta" {
		t.Fatalf("ByURL returned %q", c.Name)
	}

	// If it still scanned, ListClusters would be called and every cluster
	// converted; a single-row lookup never asks for the list at all.
	if n := m.callsOf("ListClusters"); n != 0 {
		t.Errorf("ByURL called ListClusters %d time(s); the lookup must be one indexed row, not a scan of every cluster", n)
	}
	if n := m.callsOf("ClusterByURL"); n != 1 {
		t.Errorf("ClusterByURL was called %d time(s), want exactly one", n)
	}
}

// A URL that matches nothing must not reach the store's list either, and must
// not be confused with a store that cannot answer.
func TestByURLDoesNotListForAnAbsentURL(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	m.resetCounters()

	if _, ok, err := r.ByURL("https://nowhere.example"); ok || err != nil {
		t.Fatalf("an absent URL reported ok=%v err=%v", ok, err)
	}
	if n := m.callsOf("ListClusters"); n != 0 {
		t.Errorf("an absent URL read the whole list %d time(s)", n)
	}
}

// An empty URL is a caller mistake, and asking the store for it would match the
// clusters whose url column is the empty string.
func TestByURLTreatsAnEmptyURLAsAbsent(t *testing.T) {
	r, m := testRegistry(t)
	provisioned(t, r, m, "alpha")
	// A cluster that has never published a URL: its column is ''.
	m.resetCounters()

	c, ok, err := r.ByURL("")
	if ok || c != nil || err != nil {
		t.Errorf("an empty URL matched something: %+v %v %v", c, ok, err)
	}
	if n := m.callsOf("ClusterByURL"); n != 0 {
		t.Errorf("an empty URL reached the store %d time(s); it can match every cluster that has never published one", n)
	}
}
