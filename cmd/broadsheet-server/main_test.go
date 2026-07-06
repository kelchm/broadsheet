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

	"github.com/kelchm/broadsheet/internal/archive"
	"github.com/kelchm/broadsheet/internal/source"
	"github.com/kelchm/broadsheet/pkg/broadsheet"
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

	p, err := broadsheet.New(broadsheet.Config{
		DataDir: dir,
		Width:   64,
		Sources: []broadsheet.Source{
			{ID: "a", DisplayName: "Paper A"},
			{ID: "b", DisplayName: "Paper B"},
		},
	})
	if err != nil {
		t.Fatalf("broadsheet.New: %v", err)
	}

	srv := httptest.NewServer(newRouter(p, slog.New(slog.DiscardHandler), ""))
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
	if got := resp.Header.Get("X-Broadsheet-Source"); got != "a" {
		t.Errorf("X-Broadsheet-Source = %q, want a", got)
	}
	for _, h := range []string{"X-Broadsheet-Width", "X-Broadsheet-Height", "X-Broadsheet-Days-Old"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if resp.Header.Get("X-Broadsheet-Stale") != "" {
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
// fallback with X-Broadsheet-Stale.
func TestCurrent_AdvancesPerDevice(t *testing.T) {
	srv := newTestServer(t)

	seq := func(device string, n int) []string {
		var out []string
		for range n {
			resp := get(t, srv.URL+"/current.png?device="+device)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("/current.png = %d, want 200", resp.StatusCode)
			}
			s := resp.Header.Get("X-Broadsheet-Source")
			if resp.Header.Get("X-Broadsheet-Stale") == "true" {
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
		if got := resp.Header.Get("X-Broadsheet-Source"); got != "a" {
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
	if next := resp.Header.Get("X-Broadsheet-Next-Change"); next == "" {
		t.Error("missing X-Broadsheet-Next-Change refresh hint")
	}
	if resp.Header.Get("X-Broadsheet-Slot") == "" {
		t.Error("missing X-Broadsheet-Slot")
	}

	// Idempotent: a second fetch at the same instant serves the same source.
	resp2 := get(t, srv.URL+"/rotation.png?interval=1h")
	if a, b := resp.Header.Get("X-Broadsheet-Source"), resp2.Header.Get("X-Broadsheet-Source"); a != b {
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
	if got := resp.Header.Get("X-Broadsheet-Source"); got != "a" {
		t.Errorf("slot=0 source = %q, want a", got)
	}
	if resp.Header.Get("X-Broadsheet-Stale") != "" {
		t.Error("direct slot must not be substituted")
	}

	// slot=1 selects b, which has nothing archived: deterministic substitution
	// to a, marked stale.
	resp = get(t, srv.URL+"/rotation.png?slot=1")
	if got := resp.Header.Get("X-Broadsheet-Source"); got != "a" {
		t.Errorf("slot=1 source = %q, want substituted a", got)
	}
	if resp.Header.Get("X-Broadsheet-Stale") != "true" {
		t.Error("substituted slot must carry X-Broadsheet-Stale")
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

// newStoreBackedServer builds the router over a store-backed engine (real
// 606-paper catalog, 5 defaults enabled) with one edition archived for ny-nyt.
// Nothing here polls the network — the reconciler is never started.
func newStoreBackedServer(t *testing.T, adminToken string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()

	arch := &archive.Store{Root: filepath.Join(dir, "archive")}
	img := imaging.New(32, 48, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, imaging.PNG); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	if _, err := arch.Put("ny-nyt", source.Edition{
		Date: time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), Media: source.MediaImage, Data: buf.Bytes(),
	}); err != nil {
		t.Fatalf("archive.Put: %v", err)
	}

	p, err := broadsheet.New(broadsheet.Config{DataDir: dir, Width: 64})
	if err != nil {
		t.Fatalf("broadsheet.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	srv := httptest.NewServer(newRouter(p, slog.New(slog.DiscardHandler), adminToken))
	t.Cleanup(srv.Close)
	return srv
}

func TestAPI_SourcesAndStatus(t *testing.T) {
	srv := newStoreBackedServer(t, "")

	resp := get(t, srv.URL+"/api/v1/sources")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/v1/sources = %d, want 200", resp.StatusCode)
	}
	var list struct {
		Sources []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"sources"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Sources) < 500 {
		t.Fatalf("catalog size = %d, want the full embedded catalog", len(list.Sources))
	}
	enabled := 0
	for _, s := range list.Sources {
		if s.Enabled {
			enabled++
		}
	}
	if enabled != 5 {
		t.Errorf("enabled = %d, want the 5 defaults", enabled)
	}

	var status struct {
		Ready          bool `json:"ready"`
		SourcesEnabled int  `json:"sources_enabled"`
		CatalogSize    int  `json:"catalog_size"`
	}
	resp = get(t, srv.URL+"/api/v1/status")
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Ready || status.SourcesEnabled != 5 || status.CatalogSize < 500 {
		t.Errorf("status = %+v, want ready with 5 enabled over the full catalog", status)
	}
}

func patchJSON(t *testing.T, url, body, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestAPI_EnableDisableAppliesLive(t *testing.T) {
	srv := newStoreBackedServer(t, "")

	// usat is a non-default; enabling it must apply to the live rotation set.
	if resp := patchJSON(t, srv.URL+"/api/v1/sources/usat", `{"enabled": true}`, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH usat = %d, want 200", resp.StatusCode)
	}
	var status struct {
		SourcesEnabled int `json:"sources_enabled"`
	}
	resp := get(t, srv.URL+"/api/v1/status")
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.SourcesEnabled != 6 {
		t.Errorf("after enable: sources_enabled = %d, want 6", status.SourcesEnabled)
	}

	if resp := patchJSON(t, srv.URL+"/api/v1/sources/nope", `{"enabled": true}`, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("PATCH unknown = %d, want 404", resp.StatusCode)
	}
	if resp := patchJSON(t, srv.URL+"/api/v1/sources/usat", `{}`, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PATCH without enabled = %d, want 400", resp.StatusCode)
	}
}

func TestAPI_AdminTokenGatesMutations(t *testing.T) {
	srv := newStoreBackedServer(t, "sekrit")

	// Reads stay open.
	if resp := get(t, srv.URL+"/api/v1/sources"); resp.StatusCode != http.StatusOK {
		t.Errorf("read with token configured = %d, want 200 (reads open)", resp.StatusCode)
	}
	// Mutations require the bearer token.
	if resp := patchJSON(t, srv.URL+"/api/v1/sources/usat", `{"enabled": true}`, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PATCH without token = %d, want 401", resp.StatusCode)
	}
	if resp := patchJSON(t, srv.URL+"/api/v1/sources/usat", `{"enabled": true}`, "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PATCH with wrong token = %d, want 401", resp.StatusCode)
	}
	if resp := patchJSON(t, srv.URL+"/api/v1/sources/usat", `{"enabled": true}`, "sekrit"); resp.StatusCode != http.StatusOK {
		t.Errorf("PATCH with token = %d, want 200", resp.StatusCode)
	}
}

func TestAPI_EditionsAndDatedPaper(t *testing.T) {
	srv := newStoreBackedServer(t, "")

	resp := get(t, srv.URL+"/api/v1/sources/ny-nyt/editions")
	var editions struct {
		Editions []string `json:"editions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&editions); err != nil {
		t.Fatal(err)
	}
	if len(editions.Editions) != 1 || editions.Editions[0] != "20260630" {
		t.Fatalf("editions = %v, want [20260630]", editions.Editions)
	}

	resp = get(t, srv.URL+"/paper/ny-nyt/20260630.png")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/paper/ny-nyt/20260630.png = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("dated edition missing ETag")
	}
	if resp := get(t, srv.URL+"/paper/ny-nyt/20260629.png"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing edition = %d, want 404 (permanently absent, not retryable)", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/paper/ny-nyt/junk.png"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad date = %d, want 400", resp.StatusCode)
	}
}

func TestAPI_DisableAllSourcesIsLegalAndSurvivesRestart(t *testing.T) {
	srv := newStoreBackedServer(t, "")

	// Disable every default; the last one must NOT error or brick anything.
	for _, id := range []string{"ny-nyt", "ma-bg", "ca-lat", "ca-sfc", "can-ts"} {
		if resp := patchJSON(t, srv.URL+"/api/v1/sources/"+id, `{"enabled": false}`, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("disable %s = %d, want 200", id, resp.StatusCode)
		}
	}
	var status struct {
		SourcesEnabled int `json:"sources_enabled"`
	}
	resp := get(t, srv.URL+"/api/v1/status")
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.SourcesEnabled != 0 {
		t.Errorf("sources_enabled = %d, want 0 (empty set is legal)", status.SourcesEnabled)
	}
	// Devices get a clean error, not a 500.
	if resp := get(t, srv.URL+"/rotation.png"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("rotation with nothing enabled = %d, want 400", resp.StatusCode)
	}
	// A disabled paper's archive remains addressable.
	if resp := get(t, srv.URL+"/api/v1/sources/ny-nyt/editions"); resp.StatusCode != http.StatusOK {
		t.Errorf("editions of disabled source = %d, want 200", resp.StatusCode)
	}
	if resp := get(t, srv.URL+"/paper/ny-nyt/20260630.png"); resp.StatusCode != http.StatusOK {
		t.Errorf("dated edition of disabled source = %d, want 200", resp.StatusCode)
	}
	// Re-enable works (state not stuck).
	if resp := patchJSON(t, srv.URL+"/api/v1/sources/ny-nyt", `{"enabled": true}`, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("re-enable = %d, want 200", resp.StatusCode)
	}
}

func TestAPI_RefreshIsTokenGated(t *testing.T) {
	srv := newStoreBackedServer(t, "sekrit")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sources/ny-nyt/refresh", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("refresh without token = %d, want 401", resp.StatusCode)
	}
	// Unknown source with the right token: 404 without touching the network.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/v1/sources/nope/refresh", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("refresh unknown = %d, want 404", resp2.StatusCode)
	}
}

func TestUI_PagesRender(t *testing.T) {
	srv := newStoreBackedServer(t, "")

	resp := get(t, srv.URL+"/admin")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Source health") {
		t.Errorf("/admin = %d, want status page", resp.StatusCode)
	}

	resp = get(t, srv.URL+"/admin/papers")
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/papers = %d, want 200", resp.StatusCode)
	}
	if got := strings.Count(string(body), "<tr id=\"row-"); got < 500 {
		t.Errorf("papers page has %d rows, want the full catalog", got)
	}
	if !strings.Contains(string(body), "row-ny-nyt") {
		t.Error("papers page missing ny-nyt row")
	}

	resp = get(t, srv.URL+"/admin/builder")
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "/rotation") {
		t.Errorf("/admin/builder = %d, want builder page", resp.StatusCode)
	}

	resp = get(t, srv.URL+"/static/htmx.min.js")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") == "" {
		t.Errorf("/static/htmx.min.js = %d, want cached 200", resp.StatusCode)
	}
}

func TestUI_ToggleSwapsRow(t *testing.T) {
	srv := newStoreBackedServer(t, "")

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/papers/usat/toggle?to=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("toggle = %d, want 200", resp.StatusCode)
	}
	frag := string(body)
	if !strings.Contains(frag, `id="row-usat"`) || !strings.Contains(frag, ">disable<") {
		t.Errorf("toggle fragment should be the updated (enabled) row, got: %.200s", frag)
	}
	// The engine applied it live.
	var status struct {
		SourcesEnabled int `json:"sources_enabled"`
	}
	r2 := get(t, srv.URL+"/api/v1/status")
	_ = json.NewDecoder(r2.Body).Decode(&status)
	if status.SourcesEnabled != 6 {
		t.Errorf("sources_enabled = %d, want 6 after UI toggle", status.SourcesEnabled)
	}

	// Idempotent: repeating the same rendered button's request changes nothing.
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/papers/usat/toggle?to=true", nil)
	resp3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp3.Body.Close()
	r3 := get(t, srv.URL+"/api/v1/status")
	_ = json.NewDecoder(r3.Body).Decode(&status)
	if status.SourcesEnabled != 6 {
		t.Errorf("sources_enabled = %d after repeat, want still 6 (idempotent)", status.SourcesEnabled)
	}
}

func TestUI_TokenGateWithCookieFlow(t *testing.T) {
	srv := newStoreBackedServer(t, "sekrit")

	// Mutation without any credential: 401.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/papers/usat/toggle?to=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("toggle without credential = %d, want 401", resp.StatusCode)
	}

	// Visiting a page with ?token= plants the cookie and REDIRECTS with the
	// secret stripped from the URL (history/proxy-log hygiene).
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	pageResp, err := noFollow.Get(srv.URL + "/admin?token=sekrit")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pageResp.Body.Close() }()
	if pageResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("?token= = %d, want 303 redirect stripping the token", pageResp.StatusCode)
	}
	if loc := pageResp.Header.Get("Location"); strings.Contains(loc, "token") {
		t.Errorf("redirect Location %q still carries the token", loc)
	}
	var cookie *http.Cookie
	for _, c := range pageResp.Cookies() {
		if c.Name == adminCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("?token= should plant the admin cookie")
	}
	if cookie.SameSite != http.SameSiteStrictMode || !cookie.HttpOnly {
		t.Error("admin cookie must be SameSite=Strict and HttpOnly")
	}
	if strings.Contains(cookie.Value, "sekrit") {
		t.Error("cookie value must be a digest, never the raw secret")
	}
	// …and the wrong token neither redirects nor plants a cookie.
	wrongResp, err := noFollow.Get(srv.URL + "/admin?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wrongResp.Body.Close() }()
	if wrongResp.StatusCode != http.StatusOK {
		t.Errorf("wrong ?token= = %d, want plain 200 page", wrongResp.StatusCode)
	}
	for _, c := range wrongResp.Cookies() {
		if c.Name == adminCookie {
			t.Error("wrong ?token= must not plant a cookie")
		}
	}

	// A forged cookie value must not.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/admin/papers/usat/toggle?to=true", nil)
	req.AddCookie(&http.Cookie{Name: adminCookie, Value: "forged"}) //nolint:gosec // G124: deliberately forged client-side cookie under test
	respF, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = respF.Body.Close()
	if respF.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged cookie = %d, want 401", respF.StatusCode)
	}

	// The cookie authorizes mutations.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/admin/papers/usat/toggle?to=true", nil)
	req.AddCookie(cookie)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("toggle with cookie = %d, want 200", resp2.StatusCode)
	}
}

func TestUI_ArchivePage(t *testing.T) {
	srv := newStoreBackedServer(t, "") // ny-nyt has a 20260630 edition archived
	resp := get(t, srv.URL+"/admin/archive")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/archive = %d, want 200", resp.StatusCode)
	}
	html := string(body)
	if !strings.Contains(html, "/paper/ny-nyt/20260630.png") {
		t.Error("archive grid missing the seeded ny-nyt edition cell")
	}
	if !strings.Contains(html, "Jun 30") {
		t.Error("archive grid missing the date column header")
	}
}
