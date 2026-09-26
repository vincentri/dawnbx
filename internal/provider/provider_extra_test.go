package provider

import "testing"

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
