package freedomforum

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kelchm/broadsheet/internal/source"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// dayOfMonth extracts N from a path like /dfp/pdf30/NY_NYT.pdf.
func dayOfMonth(path string) int {
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "pdf") {
			if n, err := strconv.Atoi(strings.TrimPrefix(seg, "pdf")); err == nil {
				return n
			}
		}
	}
	return -1
}

type canned struct {
	etag string
	lm   string
	body string
}

// fakeCDN serves canned 200s for the given days-of-month, 404 otherwise, and
// honors If-None-Match with a 304.
func fakeCDN(byDay map[int]canned) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mk := func(status int, hdr http.Header, body string) *http.Response {
			if hdr == nil {
				hdr = http.Header{}
			}
			return &http.Response{
				StatusCode: status, Header: hdr, Request: r,
				Body: io.NopCloser(strings.NewReader(body)),
			}
		}
		c, ok := byDay[dayOfMonth(r.URL.Path)]
		if !ok {
			return mk(http.StatusNotFound, nil, ""), nil
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == c.etag {
			return mk(http.StatusNotModified, nil, ""), nil
		}
		h := http.Header{}
		h.Set("ETag", c.etag)
		h.Set("Last-Modified", c.lm)
		return mk(http.StatusOK, h, c.body), nil
	})}
}

func TestPoll_ColdReturnsAvailableEditions(t *testing.T) {
	// now=2026-06-30 12:00 UTC -> probes pdf29 (yest), pdf30 (today), pdf1 (tom).
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	client := fakeCDN(map[int]canned{
		29: {etag: "e29", lm: "Mon, 29 Jun 2026 05:06:00 GMT", body: "%PDF-29"},
		30: {etag: "e30", lm: "Tue, 30 Jun 2026 05:06:00 GMT", body: "%PDF-30"},
		// day 1 (tomorrow) absent -> 404
	})
	ff := FreedomForum{Prefix: "NY_NYT"}

	eds, versions, err := ff.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(eds) != 2 {
		t.Fatalf("got %d editions, want 2", len(eds))
	}

	byDate := map[string]source.Edition{}
	for _, e := range eds {
		if e.Media != source.MediaPDF {
			t.Errorf("edition media = %q, want PDF", e.Media)
		}
		byDate[e.Date.Format("20060102")] = e
	}
	if e, ok := byDate["20260630"]; !ok || string(e.Data) != "%PDF-30" || e.Version != "e30" {
		t.Errorf("20260630 edition = %+v, want body %%PDF-30 / etag e30", e)
	}
	if _, ok := byDate["20260629"]; !ok {
		t.Errorf("missing 20260629 edition")
	}
	// versions must carry the two 200 folders' etags, not the 404 one.
	if len(versions) != 2 {
		t.Errorf("versions = %v, want 2 entries", versions)
	}
}

func TestPoll_WarmIsConditionalAndReturnsNothing(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	client := fakeCDN(map[int]canned{
		29: {etag: "e29", lm: "Mon, 29 Jun 2026 05:06:00 GMT", body: "%PDF-29"},
		30: {etag: "e30", lm: "Tue, 30 Jun 2026 05:06:00 GMT", body: "%PDF-30"},
	})
	ff := FreedomForum{Prefix: "NY_NYT"}

	// Seed seen with the etags for both live folders.
	seen := map[string]string{
		ff.url(now.AddDate(0, 0, -1)): "e29",
		ff.url(now):                   "e30",
	}
	eds, versions, err := ff.Poll(context.Background(), source.Deps{HTTP: client}, seen, now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(eds) != 0 {
		t.Fatalf("got %d editions, want 0 (all 304)", len(eds))
	}
	// Unchanged folders keep their tokens.
	if versions[ff.url(now)] != "e30" {
		t.Errorf("today's version = %q, want retained e30", versions[ff.url(now)])
	}
}

func TestPoll_NonPDFBodyIsProbeErrorAndRetainsToken(t *testing.T) {
	// A 200 whose body isn't a PDF (HTML error page, captive portal, empty
	// body) must not become an edition, and the old token must be retained so
	// the next poll retries instead of 304ing past the real file.
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	client := fakeCDN(map[int]canned{
		30: {etag: "e-bad", lm: "Tue, 30 Jun 2026 05:06:00 GMT", body: "<html>oops</html>"},
	})
	ff := FreedomForum{Prefix: "NY_NYT"}

	seen := map[string]string{ff.url(now): "e-old"}
	eds, versions, err := ff.Poll(context.Background(), source.Deps{HTTP: client}, seen, now)
	if err != nil {
		t.Fatalf("Poll: %v (one bad probe must not sink the poll)", err)
	}
	if len(eds) != 0 {
		t.Fatalf("got %d editions, want 0 (non-PDF body rejected)", len(eds))
	}
	if versions[ff.url(now)] != "e-old" {
		t.Errorf("today's version = %q, want retained e-old (not burned e-bad)", versions[ff.url(now)])
	}
}

func TestPoll_MissingLastModifiedFallsBackToProbeDate(t *testing.T) {
	// No Last-Modified: the edition date must fall back to the probed folder's
	// date, never a zero time (the archive rejects zero dates outright).
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	client := fakeCDN(map[int]canned{
		30: {etag: "e30", lm: "", body: "%PDF-30"},
	})
	ff := FreedomForum{Prefix: "NY_NYT"}

	eds, _, err := ff.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(eds) != 1 {
		t.Fatalf("got %d editions, want 1", len(eds))
	}
	if eds[0].Date.IsZero() {
		t.Fatal("edition date is zero; must fall back to the probe folder's date")
	}
	if got := eds[0].Date.Format("20060102"); got != "20260630" {
		t.Errorf("edition date = %s, want 20260630 (the probed folder)", got)
	}
}

func TestPoll_UnparseableLastModifiedFallsBackToProbeDate(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	client := fakeCDN(map[int]canned{
		30: {etag: "e30", lm: "not a date", body: "%PDF-30"},
	})
	ff := FreedomForum{Prefix: "NY_NYT"}

	eds, _, err := ff.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(eds) != 1 {
		t.Fatalf("got %d editions, want 1", len(eds))
	}
	if got := eds[0].Date.Format("20060102"); got != "20260630" {
		t.Errorf("edition date = %s, want 20260630 (probe-date fallback)", got)
	}
}

func TestPoll_PDFHeaderWithinFirstKilobyteIsAccepted(t *testing.T) {
	// The PDF spec's reader tolerance allows junk before the %PDF header within
	// the first 1024 bytes (BOMs, stray whitespace); the sniff must not reject
	// such files on every poll.
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	client := fakeCDN(map[int]canned{
		30: {etag: "e30", lm: "Tue, 30 Jun 2026 05:06:00 GMT", body: "\xef\xbb\xbf junk %PDF-1.4 rest"},
	})
	ff := FreedomForum{Prefix: "NY_NYT"}

	eds, _, err := ff.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(eds) != 1 {
		t.Fatalf("got %d editions, want 1 (header within first 1KB accepted)", len(eds))
	}
}
