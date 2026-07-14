package washingtonpost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kelchm/broadsheet/internal/source"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// keyOf turns a request path like /20260713/A01_SU_EZ_DAILY_20260713.pdf into
// the "20260713/SU" key the fake CDN is indexed by (date folder + zone).
func keyOf(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) != 2 {
		return ""
	}
	date := segs[0]
	parts := strings.Split(strings.TrimSuffix(segs[1], ".pdf"), "_")
	if len(parts) < 2 {
		return ""
	}
	return date + "/" + parts[1] // date + zone
}

type canned struct {
	etag string
	body string
}

// fakeCDN serves canned 200s for the given date/zone keys, a 403 (S3
// AccessDenied, as the real CDN does for a missing key) otherwise, and honors
// If-None-Match with a 304.
func fakeCDN(byKey map[string]canned) *http.Client {
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
		c, ok := byKey[keyOf(r.URL.Path)]
		if !ok {
			return mk(http.StatusForbidden, nil,
				`<?xml version="1.0"?><Error><Code>AccessDenied</Code></Error>`), nil
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" && inm == c.etag {
			return mk(http.StatusNotModified, nil, ""), nil
		}
		h := http.Header{}
		h.Set("ETag", c.etag)
		return mk(http.StatusOK, h, c.body), nil
	})}
}

// now is a fixed Monday noon UTC; the probe window is {07-12, 07-13, 07-14}.
var now = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func TestPoll_ColdPicksWhicheverZoneExists(t *testing.T) {
	// Real-world shape: today (13th) is published under zone SU, yesterday (12th)
	// under RE, tomorrow (14th) not yet published. The provider must find both
	// and never care which zone a given day used.
	client := fakeCDN(map[string]canned{
		"20260713/SU": {etag: "e13su", body: "%PDF-13"},
		"20260712/RE": {etag: "e12re", body: "%PDF-12"},
	})
	wp := New("", nil, "", "")

	eds, versions, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
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
	if e, ok := byDate["20260713"]; !ok || string(e.Data) != "%PDF-13" || e.Version != "e13su" {
		t.Errorf("20260713 edition = %+v, want body %%PDF-13 / etag e13su", e)
	}
	if e, ok := byDate["20260712"]; !ok || string(e.Data) != "%PDF-12" || e.Version != "e12re" {
		t.Errorf("20260712 edition = %+v, want body %%PDF-12 / etag e12re", e)
	}
	// Only the two 200 URLs carry tokens; every 403 probe is dropped.
	if len(versions) != 2 {
		t.Errorf("versions = %v, want exactly the 2 present editions", versions)
	}
}

func TestPoll_WarmIsConditionalAndReturnsNothing(t *testing.T) {
	client := fakeCDN(map[string]canned{
		"20260713/SU": {etag: "e13su", body: "%PDF-13"},
		"20260712/RE": {etag: "e12re", body: "%PDF-12"},
	})
	wp := New("", nil, "", "")

	seen := map[string]string{
		wp.url(now, "SU"):                   "e13su",
		wp.url(now.AddDate(0, 0, -1), "RE"): "e12re",
	}
	eds, versions, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, seen, now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(eds) != 0 {
		t.Fatalf("got %d editions, want 0 (all 304)", len(eds))
	}
	if versions[wp.url(now, "SU")] != "e13su" {
		t.Errorf("today's SU version = %q, want retained e13su", versions[wp.url(now, "SU")])
	}
}

func TestPoll_ForbiddenIsAbsentNotError(t *testing.T) {
	// Nothing published for any probed day/zone: 403s everywhere. That is a
	// healthy (if empty) poll, not a failure, and any stale token is dropped so
	// the next poll re-probes cleanly.
	client := fakeCDN(map[string]canned{})
	wp := New("", nil, "", "")

	seen := map[string]string{wp.url(now, "SU"): "e-old"}
	eds, versions, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, seen, now)
	if err != nil {
		t.Fatalf("Poll: %v (403 is absent, not an error)", err)
	}
	if len(eds) != 0 {
		t.Fatalf("got %d editions, want 0", len(eds))
	}
	if _, ok := versions[wp.url(now, "SU")]; ok {
		t.Errorf("absent URL kept a token %q; want it dropped", versions[wp.url(now, "SU")])
	}
}

func TestPoll_NonPDFBodyIsProbeErrorAndRetainsToken(t *testing.T) {
	// A 200 whose body isn't a PDF must not become an edition, and the old token
	// must be retained so the next poll retries instead of 304ing past the real
	// file.
	client := fakeCDN(map[string]canned{
		"20260713/SU": {etag: "e-bad", body: "<html>oops</html>"},
	})
	wp := New("", nil, "", "")

	seen := map[string]string{wp.url(now, "SU"): "e-old"}
	eds, versions, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, seen, now)
	if err != nil {
		t.Fatalf("Poll: %v (one bad probe must not sink the poll)", err)
	}
	if len(eds) != 0 {
		t.Fatalf("got %d editions, want 0 (non-PDF body rejected)", len(eds))
	}
	if versions[wp.url(now, "SU")] != "e-old" {
		t.Errorf("SU version = %q, want retained e-old (not burned e-bad)", versions[wp.url(now, "SU")])
	}
}

func TestPoll_EditionDateComesFromFolderNotClock(t *testing.T) {
	// The edition date is the folder date encoded in the URL, exact and never
	// zero — regardless of any Last-Modified header (the provider ignores it).
	client := fakeCDN(map[string]canned{
		"20260712/SU": {etag: "e12", body: "%PDF-12"},
	})
	wp := New("", nil, "", "")

	eds, _, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(eds) != 1 {
		t.Fatalf("got %d editions, want 1", len(eds))
	}
	if eds[0].Date.IsZero() {
		t.Fatal("edition date is zero; the archive rejects zero dates")
	}
	if got := eds[0].Date.Format("20060102"); got != "20260712" {
		t.Errorf("edition date = %s, want 20260712 (the folder date)", got)
	}
}

func TestPoll_AllProbesFailIsError(t *testing.T) {
	// Every probe failing at the transport level (upstream unreachable) is the
	// one condition that surfaces as an error.
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	})}
	wp := New("", nil, "", "")

	_, _, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err == nil {
		t.Fatal("want an error when every probe fails at the transport level")
	}
}

func TestPoll_AllUnexpectedStatusIsError(t *testing.T) {
	// A total upstream outage that still speaks HTTP (every probe 5xx) must
	// surface as a failure, not a silent healthy no-op — otherwise a prolonged
	// outage looks like a run of healthy polls in the health record.
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader("boom")),
		}, nil
	})}
	wp := New("", nil, "", "")

	_, _, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err == nil {
		t.Fatal("want an error when every probe returns an unexpected status")
	}
}

func TestPoll_OneUnexpectedStatusAmongHitsIsHealthy(t *testing.T) {
	// A single odd status among otherwise-determinate probes is not a poll
	// failure: only an all-failed poll errors. Today's SU serves a real PDF;
	// every other probe 5xx's.
	wp := New("", nil, "", "")
	todaySU := wp.url(now, "SU")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == todaySU {
			h := http.Header{}
			h.Set("ETag", "e13")
			return &http.Response{StatusCode: http.StatusOK, Header: h, Request: r,
				Body: io.NopCloser(strings.NewReader("%PDF-13"))}, nil
		}
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Request: r,
			Body: io.NopCloser(strings.NewReader("bad gateway"))}, nil
	})}

	eds, _, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err != nil {
		t.Fatalf("Poll: %v (a partial success must not error)", err)
	}
	if len(eds) != 1 {
		t.Fatalf("got %d editions, want 1 (the one 200 among 5xx)", len(eds))
	}
}

func TestPoll_PDFHeaderWithinFirstKilobyteIsAccepted(t *testing.T) {
	client := fakeCDN(map[string]canned{
		"20260713/SU": {etag: "e13", body: "\xef\xbb\xbf junk %PDF-1.4 rest"},
	})
	wp := New("", nil, "", "")

	eds, _, err := wp.Poll(context.Background(), source.Deps{HTTP: client}, nil, now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(eds) != 1 {
		t.Fatalf("got %d editions, want 1 (header within first 1KB accepted)", len(eds))
	}
}

func TestNew_DefaultsAndOverrides(t *testing.T) {
	d := New("", nil, "", "")
	if d.Page != "A01" || d.Edition != "EZ" || d.Product != "DAILY" {
		t.Errorf("defaults = %+v, want A01/EZ/DAILY", d)
	}
	if len(d.Zones) != 2 || d.Zones[0] != "SU" || d.Zones[1] != "RE" {
		t.Errorf("default zones = %v, want [SU RE]", d.Zones)
	}
	o := New("Z09", []string{"DC"}, "MD", "SUNDAY")
	if o.Page != "Z09" || o.Edition != "MD" || o.Product != "SUNDAY" || len(o.Zones) != 1 || o.Zones[0] != "DC" {
		t.Errorf("overrides not honored: %+v", o)
	}
	if got := o.url(now, "DC"); got != baseURL+"/20260713/Z09_DC_MD_SUNDAY_20260713.pdf" {
		t.Errorf("url = %s", got)
	}
}
