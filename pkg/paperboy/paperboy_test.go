package paperboy

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/disintegration/imaging"

	"github.com/kelchm/paperboy/internal/archive"
	"github.com/kelchm/paperboy/internal/source"
)

func TestMemCursors_AdvancePerDevice(t *testing.T) {
	c := newMemCursors()

	// One device cycles through the sources and wraps.
	got := []int{
		c.Next("a", 3), c.Next("a", 3), c.Next("a", 3), c.Next("a", 3),
	}
	want := []int{0, 1, 2, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("device a sequence = %v, want %v", got, want)
		}
	}

	// A different device has an independent cursor.
	if i := c.Next("b", 3); i != 0 {
		t.Errorf("device b first index = %d, want 0 (independent)", i)
	}

	// n == 0 is guarded.
	if i := c.Next("a", 0); i != 0 {
		t.Errorf("Next with n=0 = %d, want 0", i)
	}
}

func TestCompose_Dimensions(t *testing.T) {
	page := imaging.New(400, 600, color.Gray{Y: 128}) // a portrait "page"
	const master = 400

	// Both w and h: output is exactly that canvas, for contain and cover.
	contain := compose(page, RenderOptions{OutputWidth: 300, OutputHeight: 500, Fit: FitContain}, master)
	if contain.Bounds().Dx() != 300 || contain.Bounds().Dy() != 500 {
		t.Errorf("contain w&h = %dx%d, want 300x500", contain.Bounds().Dx(), contain.Bounds().Dy())
	}
	if out := compose(page, RenderOptions{OutputWidth: 500, OutputHeight: 300, Fit: FitCover}, master); out.Bounds().Dx() != 500 || out.Bounds().Dy() != 300 {
		t.Errorf("cover w&h = %dx%d, want 500x300", out.Bounds().Dx(), out.Bounds().Dy())
	}

	// Width only: width matches; height follows the page aspect (no margin here).
	if out := compose(page, RenderOptions{OutputWidth: 200, MarginPct: -1}, master); out.Bounds().Dx() != 200 || out.Bounds().Dy() != 300 {
		t.Errorf("width-only = %dx%d, want 200x300", out.Bounds().Dx(), out.Bounds().Dy())
	}

	// contain (portrait page into a taller canvas) leaves a white margin — the
	// corner is background, not page ink.
	if r, g, b, _ := contain.At(2, 2).RGBA(); r>>8 < 250 || g>>8 < 250 || b>>8 < 250 {
		t.Errorf("expected white margin at corner, got r=%d g=%d b=%d", r>>8, g>>8, b>>8)
	}
}

// uniformPNG returns PNG bytes for a w x h image of the given gray level.
func uniformPNG(t *testing.T, w, h int, level uint8) []byte {
	t.Helper()
	img := imaging.New(w, h, color.NRGBA{R: level, G: level, B: level, A: 255})
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	return buf.Bytes()
}

// newTestPaperboy builds an engine over a temp DataDir with one MediaImage
// edition archived for source "a". Providers are nil: these tests never poll.
func newTestPaperboy(t *testing.T, width int, srcIDs ...string) (*Paperboy, *archive.Store, time.Time) {
	t.Helper()
	dir := t.TempDir()
	date := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)

	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	if _, err := arch.Put("a", source.Edition{
		Date: date, Media: source.MediaImage, Data: uniformPNG(t, 32, 48, 0), // black
	}); err != nil {
		t.Fatalf("archive.Put: %v", err)
	}

	srcs := make([]Source, 0, len(srcIDs))
	for _, id := range srcIDs {
		srcs = append(srcs, Source{ID: id, DisplayName: id})
	}
	p, err := New(Config{DataDir: dir, Width: width, Sources: srcs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, arch, date
}

func TestServe_CacheInvalidatesOnArchiveOverwrite(t *testing.T) {
	p, arch, date := newTestPaperboy(t, 64, "a")

	res, err := p.RenderFor(context.Background(), "a")
	if err != nil {
		t.Fatalf("RenderFor: %v", err)
	}
	if lum := centerLuminance(t, res.Image); lum > 40 {
		t.Fatalf("first render luminance = %d, want dark (black source)", lum)
	}

	// A corrected edition is re-posted: same day, different pixels. Bump the
	// artifact's mtime well past the cached PNG's so freshness is unambiguous.
	if _, err := arch.Put("a", source.Edition{
		Date: date, Media: source.MediaImage, Data: uniformPNG(t, 32, 48, 255), // white
	}); err != nil {
		t.Fatalf("archive.Put overwrite: %v", err)
	}
	entry, _ := arch.Newest("a")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(entry.Path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	res, err = p.RenderFor(context.Background(), "a")
	if err != nil {
		t.Fatalf("RenderFor after overwrite: %v", err)
	}
	if lum := centerLuminance(t, res.Image); lum < 200 {
		t.Fatalf("post-overwrite luminance = %d, want bright: stale cache served after archive overwrite", lum)
	}
}

func TestServe_CacheKeyIncludesMasterWidth(t *testing.T) {
	p, _, _ := newTestPaperboy(t, 64, "a")
	if _, err := p.RenderFor(context.Background(), "a"); err != nil {
		t.Fatalf("RenderFor: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(p.cacheDir, "a", "*.w64.png"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("cache glob *.w64.png = %v (err %v), want exactly one width-keyed PNG", matches, err)
	}
}

// centerLuminance decodes PNG bytes and returns the red channel of the center
// pixel (the image is grayscale, so R==G==B).
func centerLuminance(t *testing.T, png []byte) uint32 {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(png))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	b := img.Bounds()
	r, _, _, _ := img.At((b.Min.X+b.Max.X)/2, (b.Min.Y+b.Max.Y)/2).RGBA()
	return r >> 8
}

// fakeRasterizer stands in for MuPDF: it counts invocations, dawdles long
// enough for concurrent callers to pile into the singleflight, records the ctx
// state it ran under, and writes a small valid PNG.
type fakeRasterizer struct {
	mu       sync.Mutex
	calls    int
	delay    time.Duration
	ctxError error
}

func (f *fakeRasterizer) Rasterize(ctx context.Context, _, pngPath string, width int) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	f.ctxError = ctx.Err()
	f.mu.Unlock()
	return imaging.Save(imaging.New(width, width, color.NRGBA{R: 128, G: 128, B: 128, A: 255}), pngPath)
}

// newPDFTestPaperboy archives one MediaPDF edition for source "a" and swaps in
// the given fake rasterizer.
func newPDFTestPaperboy(t *testing.T, fake *fakeRasterizer) *Paperboy {
	t.Helper()
	dir := t.TempDir()
	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	if _, err := arch.Put("a", source.Edition{
		Date:  time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		Media: source.MediaPDF, Data: []byte("%PDF-fake"),
	}); err != nil {
		t.Fatalf("archive.Put: %v", err)
	}
	p, err := New(Config{DataDir: dir, Width: 64, Sources: []Source{{ID: "a", DisplayName: "A"}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.renderer.Rasterizer = fake
	return p
}

func TestServe_SingleflightCollapsesConcurrentRenders(t *testing.T) {
	fake := &fakeRasterizer{delay: 150 * time.Millisecond}
	p := newPDFTestPaperboy(t, fake)

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = p.RenderFor(context.Background(), "a")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls != 1 {
		t.Errorf("rasterizer ran %d times for %d concurrent requests, want 1 (singleflight)", fake.calls, n)
	}
}

func TestServe_RenderDetachedFromCallerContext(t *testing.T) {
	// A canceled caller (client disconnect) must not abort the shared
	// cache-fill render: the flight's result serves other waiters and the
	// cache. The render runs under a detached, self-bounded context.
	fake := &fakeRasterizer{}
	p := newPDFTestPaperboy(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.RenderFor(ctx, "a"); err != nil {
		t.Fatalf("RenderFor with canceled ctx: %v (render must be detached)", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.ctxError != nil {
		t.Errorf("rasterizer saw ctx error %v, want detached (nil)", fake.ctxError)
	}
}
