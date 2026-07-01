package source

import "testing"

func TestByID(t *testing.T) {
	srcs := []Source{
		{ID: "ny-nyt", DisplayName: "The New York Times"},
		{ID: "dc-wp", DisplayName: "The Washington Post"},
	}
	if got := ByID(srcs, "ny-nyt"); got == nil || got.DisplayName != "The New York Times" {
		t.Errorf("ByID(ny-nyt) = %+v, want The New York Times", got)
	}
	if got := ByID(srcs, "does-not-exist"); got != nil {
		t.Errorf("ByID(does-not-exist) = %+v, want nil", got)
	}
}
