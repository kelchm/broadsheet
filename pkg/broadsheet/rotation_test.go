package broadsheet

import (
	"context"
	"errors"
	"image/color"
	"path/filepath"
	"testing"
	"time"

	"github.com/disintegration/imaging"

	"github.com/kelchm/broadsheet/internal/archive"
	"github.com/kelchm/broadsheet/internal/source"
)

var rotNow = time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC) // exactly on a 30m boundary

// newRotationEngine builds an engine with the given source IDs (in order),
// archiving a MediaImage edition only for those in withContent, and pins the
// clock to rotNow.
func newRotationEngine(t *testing.T, withContent map[string]bool, ids ...string) *Engine {
	t.Helper()
	dir := t.TempDir()
	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	date := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	srcs := make([]Source, 0, len(ids))
	for _, id := range ids {
		srcs = append(srcs, Source{ID: id, DisplayName: id})
		if withContent[id] {
			if _, err := arch.Put(id, source.Edition{
				Date: date, Media: source.MediaImage, Data: uniformPNG(t, 128),
			}); err != nil {
				t.Fatalf("archive.Put(%s): %v", id, err)
			}
		}
	}
	p, err := New(Config{DataDir: dir, Width: 64, Sources: srcs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.now = func() time.Time { return rotNow }
	return p
}

func TestResolveRotation_SlotMathAndBoundary(t *testing.T) {
	p := newRotationEngine(t, map[string]bool{"a": true, "b": true}, "a", "b")
	spec := RotationSpec{Interval: 30 * time.Minute}

	rot, err := p.ResolveRotation(spec)
	if err != nil {
		t.Fatalf("ResolveRotation: %v", err)
	}
	wantSlot := rotNow.Unix() / 1800
	if rot.Slot != wantSlot {
		t.Errorf("Slot = %d, want %d", rot.Slot, wantSlot)
	}
	if want := rotNow.Add(30 * time.Minute); !rot.NextChange.Equal(want) {
		t.Errorf("NextChange = %v, want %v", rot.NextChange, want)
	}
	if rot.Substituted {
		t.Error("selected source has content; must not be substituted")
	}

	// phase=1 selects the other source at the same instant; the same spec is
	// deterministic across calls.
	rot1, err := p.ResolveRotation(RotationSpec{Interval: 30 * time.Minute, Phase: 1})
	if err != nil {
		t.Fatalf("ResolveRotation phase=1: %v", err)
	}
	if rot1.SourceID == rot.SourceID {
		t.Errorf("phase=1 selected %q, want the other source (phase=0 got %q)", rot1.SourceID, rot.SourceID)
	}
	again, _ := p.ResolveRotation(spec)
	if again != rot {
		t.Errorf("resolve is not deterministic: %+v vs %+v", again, rot)
	}
}

func TestResolveRotation_NegativePhaseAndExplicitSlot(t *testing.T) {
	p := newRotationEngine(t, map[string]bool{"a": true, "b": true, "c": true}, "a", "b", "c")

	if _, err := p.ResolveRotation(RotationSpec{Phase: -7}); err != nil {
		t.Fatalf("negative phase: %v", err)
	}

	// An explicit slot pins the selection regardless of the clock: slot 0, 1, 2
	// walk the source list in order, and negative slots wrap correctly.
	for i, want := range []string{"a", "b", "c"} {
		slot := int64(i)
		rot, err := p.ResolveRotation(RotationSpec{Slot: &slot})
		if err != nil {
			t.Fatalf("slot %d: %v", slot, err)
		}
		if rot.SourceID != want {
			t.Errorf("slot %d -> %q, want %q", slot, rot.SourceID, want)
		}
	}
	neg := int64(-1)
	rot, err := p.ResolveRotation(RotationSpec{Slot: &neg})
	if err != nil || rot.SourceID != "c" {
		t.Errorf("slot -1 -> %q (err %v), want c (wraps)", rot.SourceID, err)
	}
}

func TestResolveRotation_SkipsEmptySourcesDeterministically(t *testing.T) {
	p := newRotationEngine(t, map[string]bool{"a": true}, "b", "a") // b has nothing archived

	slot := int64(0) // selects b
	rot, err := p.ResolveRotation(RotationSpec{Slot: &slot})
	if err != nil {
		t.Fatalf("ResolveRotation: %v", err)
	}
	if rot.SourceID != "a" || !rot.Substituted {
		t.Errorf("empty slot -> %q substituted=%v, want a substituted=true", rot.SourceID, rot.Substituted)
	}

	one := int64(1) // selects a directly
	rot, _ = p.ResolveRotation(RotationSpec{Slot: &one})
	if rot.SourceID != "a" || rot.Substituted {
		t.Errorf("direct slot -> %q substituted=%v, want a substituted=false", rot.SourceID, rot.Substituted)
	}
}

func TestResolveRotation_Errors(t *testing.T) {
	p := newRotationEngine(t, nil, "a", "b") // nothing archived at all
	if _, err := p.ResolveRotation(RotationSpec{}); !errors.Is(err, ErrNoneAvailable) {
		t.Errorf("empty archive: err = %v, want ErrNoneAvailable", err)
	}
	if _, err := p.ResolveRotation(RotationSpec{Sources: []string{"nope"}}); !errors.Is(err, ErrNoSourcesMatch) {
		t.Errorf("bad filter: err = %v, want ErrNoSourcesMatch", err)
	}
}

func TestRenderRotation_IsAPureRead(t *testing.T) {
	p := newRotationEngine(t, map[string]bool{"a": true, "b": true}, "a", "b")
	spec := RotationSpec{Interval: time.Hour}

	first, rot1, err := p.RenderRotation(context.Background(), spec, RenderOptions{})
	if err != nil {
		t.Fatalf("RenderRotation: %v", err)
	}
	for i := range 3 {
		res, rot, err := p.RenderRotation(context.Background(), spec, RenderOptions{})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.SourceID != first.SourceID || rot != rot1 {
			t.Fatalf("call %d changed the answer: %q/%+v vs %q/%+v — reads must not mutate rotation state",
				i, res.SourceID, rot, first.SourceID, rot1)
		}
	}
}

func TestResult_ETagVariesWithParamsOnly(t *testing.T) {
	p := newRotationEngine(t, map[string]bool{"a": true}, "a")

	r1, err := p.RenderFor(context.Background(), "a")
	if err != nil {
		t.Fatalf("RenderFor: %v", err)
	}
	if len(r1.ETag) < 4 || r1.ETag[0] != '"' {
		t.Fatalf("ETag = %q, want a non-empty quoted validator", r1.ETag)
	}
	r2, _ := p.RenderFor(context.Background(), "a")
	if r2.ETag != r1.ETag {
		t.Errorf("identical requests: ETag %q != %q", r2.ETag, r1.ETag)
	}
	r3, _ := p.RenderFor(context.Background(), "a", RenderOptions{OutputWidth: 300})
	if r3.ETag == r1.ETag {
		t.Error("different render params must produce a different ETag")
	}
}

func TestCompose_CoverNeverUpscales(t *testing.T) {
	// A canvas larger than the page with fit=cover must not interpolate the
	// page up; the native-resolution page is centered with background around it.
	page := imaging.New(400, 600, color.Gray{Y: 100})
	out := compose(page, RenderOptions{OutputWidth: 800, OutputHeight: 900, Fit: FitCover, MarginPct: -1}, 400)
	if out.Bounds().Dx() != 800 || out.Bounds().Dy() != 900 {
		t.Fatalf("dims = %dx%d, want 800x900", out.Bounds().Dx(), out.Bounds().Dy())
	}
	// Center is page ink; a point outside the 400x600 centered region is background.
	if r, _, _, _ := out.At(400, 450).RGBA(); r>>8 > 150 {
		t.Errorf("center = %d, want page gray (~100)", r>>8)
	}
	if r, _, _, _ := out.At(100, 100).RGBA(); r>>8 < 250 {
		t.Errorf("outside page region = %d, want white background (no upscale)", r>>8)
	}
}

func TestResolveRotation_SubSecondIntervalUsesDefault(t *testing.T) {
	p := newRotationEngine(t, map[string]bool{"a": true}, "a")
	// Must not panic (integer divide by zero) and must behave as unset.
	rot, err := p.ResolveRotation(RotationSpec{Interval: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("ResolveRotation: %v", err)
	}
	if want := rotNow.Add(DefaultRotationInterval); !rot.NextChange.Equal(want) {
		t.Errorf("NextChange = %v, want %v (default interval)", rot.NextChange, want)
	}
}
