package broadsheet

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/disintegration/imaging"

	"github.com/kelchm/broadsheet/internal/archive"
	"github.com/kelchm/broadsheet/internal/catalog"
	"github.com/kelchm/broadsheet/internal/source"
	"github.com/kelchm/broadsheet/internal/store"
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

// uniformPNG returns PNG bytes for a small fixture image of the given gray level.
func uniformPNG(t *testing.T, level uint8) []byte {
	t.Helper()
	img := imaging.New(32, 48, color.NRGBA{R: level, G: level, B: level, A: 255})
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
		t.Fatalf("encode fixture png: %v", err)
	}
	return buf.Bytes()
}

// newTestEngine builds an engine over a temp DataDir with one MediaImage
// edition archived for source "a". Providers are nil: these tests never poll.
func newTestEngine(t *testing.T, srcIDs ...string) (*Engine, *archive.Store, time.Time) {
	t.Helper()
	const width = 64
	dir := t.TempDir()
	date := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)

	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	if _, err := arch.Put("a", source.Edition{
		Date: date, Media: source.MediaImage, Data: uniformPNG(t, 0), // black
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

func TestKnownSource_ArchivedHistoryOutlivesCatalog(t *testing.T) {
	// No configured sources, but "a" has an archived edition — the shape of a
	// paper dropped from the catalog whose archive hasn't yet aged out. Catalog
	// membership and archive retention are independent, so "a" stays addressable
	// (listable + renderable); an id with neither a config row nor an archive is
	// genuinely unknown.
	p, arch, date := newTestEngine(t) // archives "a", configures nothing
	// "a" archived under its real name while it was active — the archive is
	// self-describing, so its identity survives leaving the catalog.
	if err := arch.SetName("a", "The A Paper"); err != nil {
		t.Fatalf("SetName: %v", err)
	}

	if !p.knownSource("a") {
		t.Fatal("archived-but-unconfigured source should stay known")
	}
	eds, err := p.ListEditions("a")
	if err != nil || len(eds) != 1 || !eds[0].Equal(date) {
		t.Fatalf("ListEditions(a) = %v, %v; want the one archived date", eds, err)
	}
	if _, err := p.RenderEdition(context.Background(), "a", date); err != nil {
		t.Fatalf("RenderEdition(a) must render archived history: %v", err)
	}
	// Its history is labeled with the real name, not a bare id — sourced from the
	// archive itself, since no catalog row remains.
	if got := p.ArchiveName("a"); got != "The A Paper" {
		t.Errorf("ArchiveName(a) = %q, want the archived label 'The A Paper'", got)
	}
	if p.knownSource("z") {
		t.Error("an id with no config row and no archive must be unknown")
	}
}

func TestBackfillArchiveLabels_LabelsExistingArchivesAtStartup(t *testing.T) {
	// An existing deploy: a catalog paper ("ny-nyt") already has an archive
	// written before self-describing metadata existed — no label. Booting the
	// store-backed engine must backfill the label from the catalog name, so the
	// paper's history keeps its name if it's later dropped (and disabled papers,
	// which never re-archive, are covered the same way).
	dir := t.TempDir()
	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	if _, err := arch.Put("ny-nyt", source.Edition{
		Date:  time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		Media: source.MediaImage, Data: uniformPNG(t, 0),
	}); err != nil {
		t.Fatalf("archive.Put: %v", err)
	}
	if got := arch.Name("ny-nyt"); got != "" {
		t.Fatalf("precondition: archive should be unlabeled, got %q", got)
	}

	p, err := New(Config{DataDir: dir}) // store-backed: seeds the embedded catalog
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.ArchiveName("ny-nyt"); got != "The New York Times" {
		t.Errorf("ArchiveName after backfill = %q, want the catalog name", got)
	}
}

func TestLoadSources_LabelsDroppedPaperBeforePruning(t *testing.T) {
	// The ordering guarantee: a paper present in an existing install's store but
	// NOT in the current catalog (dropped in this release), with an unlabeled
	// archive, must have its name stamped onto the archive BEFORE the reconcile
	// prune deletes its row — so its history keeps its real name, not a bare id.
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "broadsheet.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.SeedSources([]store.SourceRow{{
		ID: "gone-paper", DisplayName: "The Gone Gazette",
		ProviderType: "freedomforum", ProviderConfig: json.RawMessage(`{"prefix":"X"}`),
	}}); err != nil {
		t.Fatalf("seed pre-existing row: %v", err)
	}
	_ = st.Close()

	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	if _, err := arch.Put("gone-paper", source.Edition{
		Date:  time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		Media: source.MediaImage, Data: uniformPNG(t, 0),
	}); err != nil {
		t.Fatalf("archive.Put: %v", err)
	}

	// New() reconciles the embedded catalog (which has no "gone-paper"), so its
	// row is pruned — but its archive was labeled first.
	p, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.ArchiveName("gone-paper"); got != "The Gone Gazette" {
		t.Errorf("ArchiveName = %q, want the name preserved before the prune", got)
	}
	if !p.knownSource("gone-paper") {
		t.Error("dropped-but-archived paper should still be addressable via its archive")
	}
}

func TestArchive_PortableDropIn(t *testing.T) {
	// The portability property: a self-describing archive directory transplanted
	// into an install — an id in no catalog and no store — is browsable,
	// renderable, and shows its real name from its own metadata. Startup labeling
	// must NOT clobber the transplanted label with a blank.
	dir := t.TempDir()
	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	if _, err := arch.Put("foreign", source.Edition{
		Date:  time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		Media: source.MediaImage, Data: uniformPNG(t, 0),
	}); err != nil {
		t.Fatalf("archive.Put: %v", err)
	}
	if err := arch.SetName("foreign", "Le Journal"); err != nil {
		t.Fatalf("SetName: %v", err)
	}

	p, err := New(Config{DataDir: dir}) // seeds the embedded catalog; "foreign" is not in it
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !p.knownSource("foreign") {
		t.Error("transplanted archive should be addressable")
	}
	if got := p.ArchiveName("foreign"); got != "Le Journal" {
		t.Errorf("ArchiveName = %q, want the transplanted self-describing label", got)
	}
	if _, ok := p.ArchiveIndex()["foreign"]; !ok {
		t.Error("transplanted archive should appear in the archive index")
	}
}

func TestServe_CacheInvalidatesOnArchiveOverwrite(t *testing.T) {
	p, arch, date := newTestEngine(t, "a")

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
		Date: date, Media: source.MediaImage, Data: uniformPNG(t, 255), // white
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
	p, _, _ := newTestEngine(t, "a")
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

// newPDFTestEngine archives one MediaPDF edition for source "a" and swaps in
// the given fake rasterizer.
func newPDFTestEngine(t *testing.T, fake *fakeRasterizer) *Engine {
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
	p := newPDFTestEngine(t, fake)

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
	// A canceled caller (client disconnect) must not abort the SHARED
	// cache-fill render — its output serves other waiters and the cache. The
	// caller's own per-request compose may honor the cancellation; what
	// matters is that the render completed detached and is reused.
	fake := &fakeRasterizer{}
	p := newPDFTestEngine(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = p.RenderFor(ctx, "a") // canceled caller: response may fail...

	fake.mu.Lock()
	if fake.ctxError != nil {
		t.Errorf("rasterizer saw ctx error %v, want detached (nil)", fake.ctxError)
	}
	calls := fake.calls
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("rasterizer calls = %d, want 1", calls)
	}

	// ...but the detached render was preserved: a live caller succeeds
	// without re-rendering.
	if _, err := p.RenderFor(context.Background(), "a"); err != nil {
		t.Fatalf("RenderFor after canceled caller: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls != 1 {
		t.Errorf("rasterizer re-ran (%d calls); the detached render should have been reused", fake.calls)
	}
}

func TestNew_SeedsCatalogAndLoadsDefaults(t *testing.T) {
	dir := t.TempDir()
	p, err := New(Config{DataDir: dir}) // no Config.Sources -> store-backed
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	entries, err := catalog.All()
	if err != nil {
		t.Fatalf("catalog.All: %v", err)
	}
	wantDefaults := 0
	for _, e := range entries {
		if e.Default {
			wantDefaults++
		}
	}

	srcs := p.ListSources()
	if len(srcs) != wantDefaults {
		t.Fatalf("got %d enabled sources, want exactly the %d catalog defaults", len(srcs), wantDefaults)
	}
	ids := map[string]bool{}
	for _, s := range srcs {
		ids[s.ID] = true
		if s.Provider == nil {
			t.Errorf("source %s decoded without a provider", s.ID)
		}
	}
	if !ids["ny-nyt"] {
		t.Error("catalog default ny-nyt missing from enabled set")
	}
	if ids["usat"] {
		t.Error("non-default usat must not be enabled on a fresh install")
	}
	if _, err := os.Stat(filepath.Join(dir, "broadsheet.db")); err != nil {
		t.Errorf("store file not created: %v", err)
	}

	// A second engine over the same DataDir reuses the seeded store.
	p2, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatalf("New (reopen): %v", err)
	}
	if len(p2.ListSources()) != len(srcs) {
		t.Errorf("reopen changed the enabled set: %d vs %d", len(p2.ListSources()), len(srcs))
	}
}

func TestNew_ImportsLegacyStateJSON(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
	  "sources": {"ny-nyt": {"last_fetch_ok": "2026-06-29T05:00:00Z",
	                          "last_fetch_err": "2026-06-28T05:00:00Z",
	                          "last_error_msg": ""}},
	  "versions": {"ny-nyt": {"url30": "e30"}}
	}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := New(Config{DataDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if v := p.store.Versions("ny-nyt"); v["url30"] != "e30" {
		t.Errorf("imported versions = %v, want url30=e30", v)
	}
	h := p.HealthSnapshot()
	if h.Sources["ny-nyt"].LastFetchOK == nil {
		t.Error("imported health missing LastFetchOK")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.imported")); err != nil {
		t.Errorf("legacy file not set aside: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Error("legacy state.json should be gone after import")
	}
}

// trackingRasterizer records the maximum number of rasterizations running at
// once — the global bound the archive grid relies on to not OOM small hosts.
type trackingRasterizer struct {
	mu      sync.Mutex
	current int
	max     int
}

func (f *trackingRasterizer) Rasterize(_ context.Context, _, pngPath string, width int) error {
	f.mu.Lock()
	f.current++
	if f.current > f.max {
		f.max = f.current
	}
	f.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	f.mu.Lock()
	f.current--
	f.mu.Unlock()
	return imaging.Save(imaging.New(width, width, color.NRGBA{R: 128, G: 128, B: 128, A: 255}), pngPath)
}

func TestServe_RasterizationsAreGloballyBounded(t *testing.T) {
	// Many DIFFERENT cold editions at once (the archive-grid shape):
	// singleflight can't collapse them, so the global semaphore must queue
	// them — each concurrent rasterization holds ~150MB+.
	dir := t.TempDir()
	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	ids := []string{"a", "b", "c", "d", "e", "f"}
	var srcs []Source
	for _, id := range ids {
		if _, err := arch.Put(id, source.Edition{
			Date:  time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
			Media: source.MediaPDF, Data: []byte("%PDF-fake"),
		}); err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, Source{ID: id, DisplayName: id})
	}
	p, err := New(Config{DataDir: dir, Width: 64, Sources: srcs})
	if err != nil {
		t.Fatal(err)
	}
	fake := &trackingRasterizer{}
	p.renderer.Rasterizer = fake

	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = p.RenderFor(context.Background(), id)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("source %s: %v", ids[i], err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.max > 1 {
		t.Errorf("max concurrent rasterizations = %d, want 1 (global bound)", fake.max)
	}
}

func TestServe_VariantCacheSkipsMasterDecode(t *testing.T) {
	p, arch, _ := newTestEngine(t, "a")

	first, err := p.RenderFor(context.Background(), "a", RenderOptions{OutputWidth: 48})
	if err != nil {
		t.Fatalf("first render: %v", err)
	}

	// Corrupt the cached master while preserving its stamped mtime: only a
	// variant-cache hit (same ETag) can serve this request now.
	entry, _ := arch.Newest("a")
	matches, _ := filepath.Glob(filepath.Join(p.cacheDir, "a", "*.png"))
	if len(matches) != 1 {
		t.Fatalf("expected one cached master, got %v", matches)
	}
	fi, err := os.Stat(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(matches[0], []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(matches[0], fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	_ = entry

	second, err := p.RenderFor(context.Background(), "a", RenderOptions{OutputWidth: 48})
	if err != nil {
		t.Fatalf("second render should hit the variant cache: %v", err)
	}
	if second.ETag != first.ETag {
		t.Errorf("ETag changed across cache hit: %q vs %q", second.ETag, first.ETag)
	}
	// Different params miss the cache and now fail on the corrupted master —
	// proving the hit above really came from the memo, not the file.
	if _, err := p.RenderFor(context.Background(), "a", RenderOptions{OutputWidth: 52}); err == nil {
		t.Error("differently-sized render should have missed the cache and failed on the corrupt master")
	}
}

func TestVariantCache_BoundedAndSizeCapped(t *testing.T) {
	c := newVariantCache(2, 100)
	big := &Result{Image: make([]byte, 200), ETag: "big"}
	c.put("big", big)
	if _, ok := c.get("big"); ok {
		t.Error("oversized entries must not be cached")
	}
	c.put("a", &Result{Image: []byte("1"), ETag: "a"})
	c.put("b", &Result{Image: []byte("2"), ETag: "b"})
	c.put("c", &Result{Image: []byte("3"), ETag: "c"}) // evicts a
	if _, ok := c.get("a"); ok {
		t.Error("capacity eviction failed")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("newest entry missing")
	}
}
