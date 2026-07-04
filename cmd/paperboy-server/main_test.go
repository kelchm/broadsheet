package main

import (
	"bytes"
	"encoding/json"
	"image/color"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/disintegration/imaging"

	"github.com/kelchm/paperboy/internal/archive"
	"github.com/kelchm/paperboy/internal/source"
	"github.com/kelchm/paperboy/pkg/paperboy"
)

// newTestServer builds the real router over an engine with a temp DataDir.
// Sources "a" and "b" are configured; only "a" has an archived edition, so the
// stale cross-source fallback is exercisable. Providers are nil — nothing here
// polls the network.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()

	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	img := imaging.New(32, 48, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	if _, err := arch.Put("a", source.Edition{
		Date: time.Now().UTC(), Media: source.MediaImage, Data: buf.Bytes(),
	}); err != nil {
		t.Fatalf("archive.Put: %v", err)
	}

	p, err := paperboy.New(paperboy.Config{
		DataDir: dir,
		Width:   64,
		Sources: []paperboy.Source{
			{ID: "a", DisplayName: "Paper A"},
			{ID: "b", DisplayName: "Paper B"},
		},
	})
	if err != nil {
		t.Fatalf("paperboy.New: %v", err)
	}

	srv := httptest.NewServer(newRouter(p, slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // G107: httptest URL
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestHealthEndpoints(t *testing.T) {
	srv := newTestServer(t)
	if resp := get(t, srv.URL+"/health"); resp.StatusCode != http.StatusOK {
		t.Errorf("/health = %d, want 200", resp.StatusCode)
	}
	// One edition is archived, so readiness is green.
	if resp := get(t, srv.URL+"/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}
}

func TestPaper_ServesImageWithHeaders(t *testing.T) {
	srv := newTestServer(t)
	resp := get(t, srv.URL+"/paper/a.png")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/paper/a.png = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if got := resp.Header.Get("X-Paperboy-Source"); got != "a" {
		t.Errorf("X-Paperboy-Source = %q, want a", got)
	}
	for _, h := range []string{"X-Paperboy-Width", "X-Paperboy-Height", "X-Paperboy-Days-Old"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if resp.Header.Get("X-Paperboy-Stale") != "" {
		t.Error("direct /paper read must never be marked stale")
	}
}

func TestPaper_UnknownSourceIs404(t *testing.T) {
	srv := newTestServer(t)
	if resp := get(t, srv.URL+"/paper/nope.png"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/paper/nope.png = %d, want 404", resp.StatusCode)
	}
}

// TestCurrent_AdvancesPerDevice characterizes the shipped advance-on-GET
// rotation contract: each load moves a device to the next source, devices are
// independent, and a source with nothing archived serves the cross-source
// fallback with X-Paperboy-Stale.
func TestCurrent_AdvancesPerDevice(t *testing.T) {
	srv := newTestServer(t)

	seq := func(device string, n int) []string {
		var out []string
		for range n {
			resp := get(t, srv.URL+"/current.png?device="+device)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("/current.png = %d, want 200", resp.StatusCode)
			}
			s := resp.Header.Get("X-Paperboy-Source")
			if resp.Header.Get("X-Paperboy-Stale") == "true" {
				s += ":stale"
			}
			out = append(out, s)
		}
		return out
	}

	// Rotation is a,b,a,...; b has nothing archived, so its slot serves the
	// newest edition of any source (a's) marked stale.
	got := seq("kitchen", 3)
	want := []string{"a", "a:stale", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("kitchen sequence = %v, want %v", got, want)
		}
	}

	// A different device starts its own cursor from the top.
	if first := seq("hall", 1)[0]; first != "a" {
		t.Errorf("new device first paper = %q, want a (independent cursor)", first)
	}
}

func TestCurrent_SourcesFilterRestrictsRotation(t *testing.T) {
	srv := newTestServer(t)
	for i := range 3 {
		resp := get(t, srv.URL+"/current.png?device=d&sources=a")
		if got := resp.Header.Get("X-Paperboy-Source"); got != "a" {
			t.Fatalf("load %d: source = %q, want pinned to a", i, got)
		}
	}
	if resp := get(t, srv.URL+"/current.png?device=d&sources=nope"); resp.StatusCode == http.StatusOK {
		t.Error("unknown sources filter should not serve an image")
	}
}

func TestCurrent_BadParamsAre400(t *testing.T) {
	srv := newTestServer(t)
	for _, q := range []string{"w=-3", "w=abc", "h=0", "fit=stretch", "margin=-1"} {
		if resp := get(t, srv.URL+"/current.png?"+q); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestParseRenderOpts_MarginZeroMeansNoMargin(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/current.png?margin=0", nil)
	opts, err := parseRenderOpts(r)
	if err != nil {
		t.Fatalf("parseRenderOpts: %v", err)
	}
	if opts.MarginPct != -1 {
		t.Errorf("margin=0 -> MarginPct = %v, want -1 (explicit no-margin sentinel)", opts.MarginPct)
	}
}

// TestDeviceID characterizes the CURRENT device-identity contract
// (?device= else client IP). The IP fallback is slated to be replaced by
// stateless time-driven rotation; update or delete alongside that change.
func TestDeviceID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/current.png?device=%20kitchen%20", nil)
	if got := deviceID(r); got != "name:kitchen" {
		t.Errorf("explicit device = %q, want name:kitchen", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/current.png", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	if got := deviceID(r); got != "ip:10.1.2.3" {
		t.Errorf("ip fallback = %q, want ip:10.1.2.3", got)
	}
}

func TestRotationPNG_IdempotentWithCachingHeaders(t *testing.T) {
	srv := newTestServer(t)

	resp := get(t, srv.URL+"/rotation.png?interval=1h")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/rotation.png = %d, want 200", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if len(etag) < 4 || etag[0] != '"' {
		t.Errorf("ETag = %q, want a quoted validator", etag)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") {
		t.Errorf("Cache-Control = %q, want public, max-age=<to boundary>", cc)
	}
	if next := resp.Header.Get("X-Paperboy-Next-Change"); next == "" {
		t.Error("missing X-Paperboy-Next-Change refresh hint")
	}
	if resp.Header.Get("X-Paperboy-Slot") == "" {
		t.Error("missing X-Paperboy-Slot")
	}

	// Idempotent: a second fetch at the same instant serves the same source.
	resp2 := get(t, srv.URL+"/rotation.png?interval=1h")
	if a, b := resp.Header.Get("X-Paperboy-Source"), resp2.Header.Get("X-Paperboy-Source"); a != b {
		t.Errorf("two reads changed the answer: %q then %q — rotation reads must not mutate", a, b)
	}

	// Conditional GET: matching If-None-Match yields 304 with no body.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/rotation.png?interval=1h", nil)
	req.Header.Set("If-None-Match", etag)
	cond, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	defer func() { _ = cond.Body.Close() }()
	if cond.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match = %d, want 304", cond.StatusCode)
	}
}

func TestRotationPNG_ExplicitSlotAndSubstitution(t *testing.T) {
	srv := newTestServer(t) // sources a (content), b (empty)

	// slot=0 selects a directly.
	resp := get(t, srv.URL+"/rotation.png?slot=0")
	if got := resp.Header.Get("X-Paperboy-Source"); got != "a" {
		t.Errorf("slot=0 source = %q, want a", got)
	}
	if resp.Header.Get("X-Paperboy-Stale") != "" {
		t.Error("direct slot must not be substituted")
	}

	// slot=1 selects b, which has nothing archived: deterministic substitution
	// to a, marked stale.
	resp = get(t, srv.URL+"/rotation.png?slot=1")
	if got := resp.Header.Get("X-Paperboy-Source"); got != "a" {
		t.Errorf("slot=1 source = %q, want substituted a", got)
	}
	if resp.Header.Get("X-Paperboy-Stale") != "true" {
		t.Error("substituted slot must carry X-Paperboy-Stale")
	}
}

func TestRotationPNG_ErrorMapping(t *testing.T) {
	srv := newTestServer(t)
	for _, q := range []string{"interval=5s", "interval=25h", "slot=abc", "phase=x"} {
		if resp := get(t, srv.URL+"/rotation.png?"+q); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, resp.StatusCode)
		}
	}
	if resp := get(t, srv.URL+"/rotation.png?sources=nope"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown sources filter = %d, want 400", resp.StatusCode)
	}
	// b is configured but has nothing archived: an empty rotation is 503
	// (retryable cold start), not a caller error.
	if resp := get(t, srv.URL+"/rotation.png?sources=b"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("empty rotation = %d, want 503", resp.StatusCode)
	}
}

func TestRotationPage_SelfPacingHTML(t *testing.T) {
	srv := newTestServer(t)
	resp := get(t, srv.URL+"/rotation?interval=30m&phase=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/rotation = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	for _, want := range []string{
		`src="/rotation.png?`, // server-rendered initial image (correct without JS)
		"okular.Sleep",        // Visionect panel-sleep integration
		`content="43200"`,     // 12h meta-refresh watchdog
	} {
		if !strings.Contains(html, want) {
			t.Errorf("display page missing %q", want)
		}
	}
	// html/template pads JS-context interpolations with spaces; match loosely.
	for name, re := range map[string]*regexp.Regexp{
		"dwell seconds": regexp.MustCompile(`var I =\s*1800\s*;`),
		"phase":         regexp.MustCompile(`var phase =\s*1\s*;`),
	} {
		if !re.MatchString(html) {
			t.Errorf("display page missing %s (%s)", name, re)
		}
	}
}

func TestTRMNLEnvelope(t *testing.T) {
	srv := newTestServer(t)
	resp := get(t, srv.URL+"/api/display?interval=30m&w=800&h=480")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/display = %d, want 200", resp.StatusCode)
	}
	var env struct {
		ImageURL       string `json:"image_url"`
		Filename       string `json:"filename"`
		RefreshRate    int    `json:"refresh_rate"`
		UpdateFirmware bool   `json:"update_firmware"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !strings.Contains(env.ImageURL, "/rotation.png?") ||
		!strings.Contains(env.ImageURL, "slot=") ||
		!strings.Contains(env.ImageURL, "w=800") ||
		!strings.HasPrefix(env.ImageURL, "http://") {
		t.Errorf("image_url = %q, want absolute slot-explicit /rotation.png URL", env.ImageURL)
	}
	if env.RefreshRate < 60 {
		t.Errorf("refresh_rate = %d, want >= 60", env.RefreshRate)
	}
	// filename is the firmware's skip-repaint identity: source + edition date
	// + extension, never the slot (a one-source rotation must not re-flash the
	// same paper every slot).
	if !strings.HasPrefix(env.Filename, "a-") || !strings.HasSuffix(env.Filename, ".png") ||
		strings.Contains(env.Filename, "slot") {
		t.Errorf("filename = %q, want a-<date>.png with no slot", env.Filename)
	}
	if env.UpdateFirmware {
		t.Error("update_firmware must be false")
	}
}

func TestPaper_ConditionalGET(t *testing.T) {
	srv := newTestServer(t)
	resp := get(t, srv.URL+"/paper/a.png")
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag on /paper")
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.HasPrefix(cc, "public") {
		t.Errorf("Cache-Control = %q, want public", cc)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/paper/a.png", nil)
	req.Header.Set("If-None-Match", etag)
	cond, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	defer func() { _ = cond.Body.Close() }()
	if cond.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match = %d, want 304", cond.StatusCode)
	}
}

func TestPaper_ConfiguredButEmptyIs503(t *testing.T) {
	// b is configured but the reconciler hasn't filled it: retryable (503),
	// not a caller error and never a 500.
	srv := newTestServer(t)
	if resp := get(t, srv.URL+"/paper/b.png"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/paper/b.png = %d, want 503", resp.StatusCode)
	}
}

func TestCurrent_UnknownSourcesFilterIs400(t *testing.T) {
	srv := newTestServer(t)
	if resp := get(t, srv.URL+"/current.png?sources=nope"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/current.png?sources=nope = %d, want 400", resp.StatusCode)
	}
}

func TestRotationPage_Validation(t *testing.T) {
	srv := newTestServer(t)
	// A typo'd sources filter must fail loudly at provisioning time, not
	// render a permanently broken e-ink frame.
	if resp := get(t, srv.URL+"/rotation?sources=nope"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/rotation?sources=nope = %d, want 400", resp.StatusCode)
	}
	// ?slot= pins a single paper and belongs on the image endpoint.
	if resp := get(t, srv.URL+"/rotation?slot=5"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("/rotation?slot=5 = %d, want 400", resp.StatusCode)
	}
	// A cold (still-filling) archive is NOT an error: the page retries.
	if resp := get(t, srv.URL+"/rotation?sources=b"); resp.StatusCode != http.StatusOK {
		t.Errorf("/rotation?sources=b (empty archive) = %d, want 200", resp.StatusCode)
	}
}

func TestEtagMatches(t *testing.T) {
	const e = `"abc123"`
	cases := []struct {
		header string
		want   bool
	}{
		{`"abc123"`, true},
		{`W/"abc123"`, true},                // weak comparison
		{`"other", "abc123"`, true},         // list membership
		{`"other", W/"abc123" , "x"`, true}, // list + weak + spacing
		{`*`, true},                         // wildcard
		{`"other"`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := etagMatches(c.header, e); got != c.want {
			t.Errorf("etagMatches(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}
