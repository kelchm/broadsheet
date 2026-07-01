package registry

import "testing"

func TestDefaultNonEmpty(t *testing.T) {
	if len(Default()) == 0 {
		t.Fatal("Default() returned no sources")
	}
}

func TestDefaultIDsUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Default() {
		if seen[s.ID] {
			t.Errorf("duplicate source ID: %q", s.ID)
		}
		seen[s.ID] = true
	}
}

func TestDefaultSourcesHaveProviders(t *testing.T) {
	for _, s := range Default() {
		if s.Provider == nil {
			t.Errorf("source %q has no provider", s.ID)
		}
	}
}
