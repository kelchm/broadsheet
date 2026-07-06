package catalog

import "testing"

func TestAll_ParsesAndIsWellFormed(t *testing.T) {
	entries, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(entries) < 6 {
		t.Fatalf("catalog has %d entries, want at least the classic 6", len(entries))
	}
	seen := map[string]bool{}
	defaults := 0
	for _, e := range entries {
		if e.ID == "" || e.Name == "" || e.Provider == "" {
			t.Errorf("entry %+v missing required fields", e)
		}
		if seen[e.ID] {
			t.Errorf("duplicate catalog id %q", e.ID)
		}
		seen[e.ID] = true
		if e.Default {
			defaults++
		}
	}
	if defaults == 0 {
		t.Error("no default entries; a fresh install would have nothing enabled")
	}
}
