package provider

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestListKeepsRegistrationOrder: the dashboard's picker is stable because the
// listing is, so the order providers were registered in is the order they are
// shown in. A caller must not be able to disturb it either.
func TestListKeepsRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	r.Register(newFake("aws"))
	r.Register(newFake("gcp"))
	r.Register(newFake("azure"))

	got := ids(r.List())
	if strings.Join(got, ",") != "aws,gcp,azure" {
		t.Errorf("List() = %v, want registration order", got)
	}
	// The returned slice is the caller's, so writing to it must not reach the
	// registry's own copy.
	for i := range got {
		got[i] = "zzz"
	}
	if again := ids(r.List()); strings.Join(again, ",") != "aws,gcp,azure" {
		t.Errorf("List() aliased the registry's own slice: %v", again)
	}
	if l := NewRegistry().List(); len(l) != 0 {
		t.Errorf("an empty registry listed %v", l)
	}
}

// ids projects a listing to the ids a test compares, so a test about order does
// not build the same string by hand at every assertion.
func ids(l []Listed) []string {
	out := make([]string, len(l))
	for i, v := range l {
		out[i] = v.ID
	}
	return out
}

// TestGetRejectsAnUnlistedProvider: an unknown id is refused rather than handing
// back nil, so a caller cannot dereference a nil provider by accident.
func TestGetRejectsAnUnlistedProvider(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Get("aws"); err == nil {
		t.Fatal("an unlisted provider was handed out")
	}
}

// TestDeclareIsListedAndRefused: a provider this build knows about but cannot
// use must be visible and unusable — visible so an operator can see the product
// has a roadmap, unusable so nothing can be provisioned with a name that would
// only fail later.
func TestDeclareIsListedAndRefused(t *testing.T) {
	r := NewRegistry()
	r.Declare("gcp")
	r.Declare("azure")
	r.Register(newFake("aws"))

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("listed %d providers, want 3: %+v", len(list), list)
	}
	// Registration order, so the dashboard does not reshuffle between reloads.
	if list[0].ID != "gcp" || list[1].ID != "azure" || list[2].ID != "aws" {
		t.Errorf("list lost its order: %+v", list)
	}
	for _, l := range list {
		want := l.ID == "aws"
		if l.Available != want {
			t.Errorf("%s available = %v, want %v", l.ID, l.Available, want)
		}
	}
	// A declared provider is refused exactly like an unknown one, so there is no
	// second way to ask for a cloud this build does not have.
	for _, id := range []string{"gcp", "azure", "nope"} {
		if _, err := r.Get(id); err == nil {
			t.Errorf("%s was handed out", id)
		}
	}
	// And there is exactly one usable provider, which is the one phase one has.
	p, err := r.Available()
	if err != nil || p == nil || p.ID() != "aws" {
		t.Fatalf("Available() = %v, %v, want the aws adapter", p, err)
	}
}

// TestAvailableOnARegistryWithNothingUsable: a control plane whose credentials
// are not working has providers and none of them usable. That is not an error,
// it is the state the dashboard has to be able to show.
func TestAvailableOnARegistryWithNothingUsable(t *testing.T) {
	r := NewRegistry()
	r.Declare("gcp")
	p, err := r.Available()
	if err != nil {
		t.Errorf("an empty registry is not an error: %v", err)
	}
	if p != nil {
		t.Errorf("nothing is usable, so Available returned %v", p)
	}
	// And the empty answer marshals as an array. A nil slice is "no answer",
	// which is a different claim from "there are none", and a client cannot tell
	// them apart once they are both rendered as a blank list.
	b, err := json.Marshal(struct {
		Providers []Listed `json:"providers"`
	}{NewRegistry().List()})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"providers":[]}` {
		t.Errorf("an empty roster marshals as %s, want an empty array", b)
	}
}

// TestAvailableRefusesTwoUsableProviders: phase one has one by design, and a
// second available provider is a coin flip the caller would never notice.
func TestAvailableRefusesTwoUsableProviders(t *testing.T) {
	r := NewRegistry()
	r.Register(newFake("aws"))
	r.Register(newFake("azure"))
	if _, err := r.Available(); err == nil {
		t.Error("two usable providers were accepted without comment")
	}
}

func TestDeclaringTwiceIsAProgrammingError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("declaring the same provider twice did not panic")
		}
	}()
	r := NewRegistry()
	r.Declare("gcp")
	r.Declare("gcp")
}

func TestDeclaringWhatIsAlreadyRegisteredIsAProgrammingError(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("declaring a registered provider did not panic")
		}
	}()
	r := NewRegistry()
	r.Register(newFake("aws"))
	r.Declare("aws")
}

// TestRegisteringWhatWasDeclaredUpgradesIt: this is the path a control plane
// takes. It declares the roadmap, then registers the adapter it managed to
// build, and the declared entry becomes available in place — keeping its place
// in the listing, so the picker does not reshuffle.
func TestRegisteringWhatWasDeclaredUpgradesIt(t *testing.T) {
	r := NewRegistry()
	r.Declare("gcp")
	r.Register(newFake("aws"))
	r.Register(newFake("gcp")) // this is the one it could build

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("registering over a declaration changed the size: %+v", list)
	}
	if list[0].ID != "gcp" || !list[0].Available {
		t.Errorf("the upgraded provider did not become available in place: %+v", list)
	}
	if p, err := r.Get("gcp"); err != nil || p.ID() != "gcp" {
		t.Errorf("an upgraded provider is still refused: %v", err)
	}
}

// TestAvailableIsSerialisedAgainstRegistration: Available walks the listing
// that Register and Declare write, so it has to hold the read lock while it
// does. It did not, which is a data race the moment anything registers after
// startup, and the race detector is not part of the gate — so this checks the
// lock, deterministically.
//
// The registry is declared-only, which is the shape that shows the gap: with
// no usable provider the loop never reaches Get, so an Available that takes no
// lock of its own reads the listing and returns while a writer still owns it.
// A registered provider would hide that, because Get blocks on the write lock
// and the read would look serialised by accident.
func TestAvailableIsSerialisedAgainstRegistration(t *testing.T) {
	r := NewRegistry()
	r.Declare("aws")
	r.Declare("gcp")

	r.mu.Lock() // as Register and Declare hold it
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Available()
	}()

	select {
	case <-done:
		r.mu.Unlock()
		t.Fatal("Available read the listing while a registration held the registry")
	case <-time.After(50 * time.Millisecond):
	}
	r.mu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Available never returned after the registry was released")
	}
}
