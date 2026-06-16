package source

import "testing"

func TestDefaultRegistryNonEmpty(t *testing.T) {
	got := Default()
	if len(got) == 0 {
		t.Fatal("Default() returned no sources")
	}
}

func TestDefaultRegistryIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Default() {
		if seen[s.ID] {
			t.Errorf("duplicate source ID: %q", s.ID)
		}
		seen[s.ID] = true
	}
}

func TestByID(t *testing.T) {
	srcs := Default()
	if got := ByID(srcs, "ny-nyt"); got == nil || got.Prefix != "NY_NYT" {
		t.Errorf("ByID(ny-nyt) = %+v, want NY_NYT", got)
	}
	if got := ByID(srcs, "does-not-exist"); got != nil {
		t.Errorf("ByID(does-not-exist) = %+v, want nil", got)
	}
}
