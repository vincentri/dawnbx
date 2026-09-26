package provider

import (
	"encoding/json"
	"testing"
)

// TestIDsIsSortedAndIndependent: IDs copies before sorting, so a caller cannot
// reorder the registry's own registration order by sorting the result. That
// matters because List uses registration order for a stable UI, and the two must
// not be able to disagree.
func TestIDsIsSortedAndIndependent(t *testing.T) {
	r := NewRegistry()
	r.Register(newFake("aws"))
	r.Register(newFake("gcp"))
	r.Register(newFake("azure"))

	got := r.IDs()
	want := "aws,azure,gcp"
	if join(got) != want {
		t.Fatalf("IDs() = %v, want %s", got, want)
	}
	// Registration order is what List uses, and it is the reverse of sorted here.
	if l := r.List(); join([]string{l[0].ID, l[1].ID, l[2].ID}) != "aws,gcp,azure" {
		t.Errorf("List() lost registration order: %s", join([]string{l[0].ID, l[1].ID, l[2].ID}))
	}
	// Sorting the returned slice must not disturb the registry.
	for i := range got {
		got[i] = "zzz"
	}
	if again := r.IDs(); join(again) != want {
		t.Errorf("IDs() aliased the registry's own slice: %v", again)
	}
}

func TestIDsOnAnEmptyRegistry(t *testing.T) {
	if got := NewRegistry().IDs(); len(got) != 0 {
		t.Errorf("an empty registry listed %v", got)
	}
	if got := NewRegistry().List(); len(got) != 0 {
		t.Errorf("an empty registry listed %v", got)
	}
}

// TestGetRejectsAnUnlistedProvider: an unknown id is refused rather than handing
// back nil, so a caller cannot dereference a nil provider by accident.
func TestGetRejectsAnUnlistedProvider(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Get("aws"); err == nil {
		t.Fatal("an unlisted provider was handed out")
	}
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
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

// TestIDsCoversDeclaredProviders: the sorted helper must see every known
// provider, not only the usable ones, or a test asserting on the roster is
// quietly checking less than it reads as checking.
func TestIDsCoversDeclaredProviders(t *testing.T) {
	r := NewRegistry()
	r.Declare("gcp")
	r.Register(newFake("aws"))
	if got := join(r.IDs()); got != "aws,gcp" {
		t.Errorf("IDs() = %q, want aws,gcp", got)
	}
}
