package registry

import (
	"testing"

	"github.com/kelchm/paperboy/internal/catalog"
)

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

func TestDecode(t *testing.T) {
	p, err := Decode("freedomforum", []byte(`{"prefix":"NY_NYT"}`))
	if err != nil || p == nil {
		t.Fatalf("Decode freedomforum: %v", err)
	}
	if _, err := Decode("freedomforum", []byte(`{}`)); err == nil {
		t.Error("missing prefix must error")
	}
	if _, err := Decode("freedomforum", []byte(`not json`)); err == nil {
		t.Error("bad JSON must error")
	}
	if _, err := Decode("mystery", []byte(`{}`)); err == nil {
		t.Error("unknown provider type must error")
	}
}

func TestEveryCatalogEntryDecodes(t *testing.T) {
	// The catalog ships enabled-able data; a typo'd provider type or empty
	// prefix must fail at build time, not vanish a paper at runtime.
	entries, err := catalog.All()
	if err != nil {
		t.Fatalf("catalog.All: %v", err)
	}
	for _, e := range entries {
		if _, err := Build(e.ID, e.Name, e.Provider, e.Config); err != nil {
			t.Errorf("catalog entry %s does not decode: %v", e.ID, err)
		}
	}
}
