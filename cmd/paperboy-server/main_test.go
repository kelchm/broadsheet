package main

import (
	"bytes"
	"image/color"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
