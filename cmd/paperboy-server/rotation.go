package main

// The time-driven device plane: /rotation.png (raw image), /rotation (the
// display page), and /api/display (TRMNL-wire-compatible envelope). All three
// are pure reads — which paper shows is a function of the clock and the URL's
// params, so any client can fetch, preview, or retry without perturbing
// anything. Every response carries a forward-looking refresh hint (seconds to
// the next slot boundary) in the transport each client class understands:
// Cache-Control/X-Paperboy-Next-Change for raw pullers, refresh_rate for
// TRMNL, and okular.Sleep on the display page for Visionect panels.

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kelchm/paperboy/pkg/paperboy"
)

// parseRotationSpec extracts rotation params from the query.
//
//	?sources=   comma-separated source IDs to rotate over (empty = all)
//	?interval=  dwell per paper, Go duration syntax ("30m", "1h", "90s");
//	            bounded [1m, 24h]; default 30m
//	?phase=     integer slot offset (same playlist, deliberately out of step)
//	?slot=      explicit absolute slot index (display pages address slots
//	            explicitly so a given URL always yields the same paper)
func parseRotationSpec(q url.Values) (paperboy.RotationSpec, error) {
	var spec paperboy.RotationSpec
	if s := q.Get("sources"); s != "" {
		spec.Sources = splitSources(s)
	}
	if s := q.Get("interval"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Minute || d > 24*time.Hour {
			return spec, fmt.Errorf("invalid interval=%q (want a duration between 1m and 24h, e.g. 30m)", s)
		}
		spec.Interval = d
	}
	if s := q.Get("phase"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return spec, fmt.Errorf("invalid phase=%q (want an integer)", s)
		}
		spec.Phase = v
	}
	if s := q.Get("slot"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return spec, fmt.Errorf("invalid slot=%q (want an integer)", s)
		}
		spec.Slot = &v
	}
	return spec, nil
}

// writeEngineError maps engine sentinel errors to HTTP statuses: caller
// mistakes are 4xx, an empty archive is 503 (retryable), everything else is a
// real server-side failure.
func writeEngineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, paperboy.ErrNoSourcesMatch):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, paperboy.ErrUnknownSource), errors.Is(err, paperboy.ErrEditionNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, paperboy.ErrNoneAvailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// secondsUntil rounds a refresh hint UP to whole seconds (min 1): a device
// that sleeps the advertised hint must wake just past the boundary, never
// fractionally before it (which would re-fetch the old slot and, for clients
// with a minimum wake period, settle into showing every paper late).
func secondsUntil(t time.Time) int {
	s := int(math.Ceil(time.Until(t).Seconds()))
	if s < 1 {
		return 1
	}
	return s
}

// etagMatches implements the subset of RFC 7232 If-None-Match handling we
// need: comma-separated candidate lists, optional W/ weak prefixes, and "*".
// Anything unparseable simply fails to match (worst case a full 200).
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" || etag == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, cand := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimPrefix(strings.TrimSpace(cand), "W/") == want {
			return true
		}
	}
	return false
}

func handleRotationPNG(p *paperboy.Paperboy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts, err := parseRenderOpts(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		spec, err := parseRotationSpec(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// parseRenderOpts also reads ?sources= (for /current.png); the spec owns
		// it here.
		opts.Sources = nil

		res, rot, err := p.RenderRotation(r.Context(), spec, opts)
		if err != nil {
			writeEngineError(w, err)
			return
		}

		hint := secondsUntil(rot.NextChange)
		w.Header().Set("ETag", res.ETag)
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", hint))
		w.Header().Set("X-Paperboy-Next-Change", strconv.Itoa(hint))
		w.Header().Set("X-Paperboy-Slot", strconv.FormatInt(rot.Slot, 10))
		if etagMatches(r.Header.Get("If-None-Match"), res.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		writeImageBody(w, res)
	}
}

// rotationTmpl is the display page for HTML-rendering displays (Visionect,
// browsers). The initial image is server-rendered into the markup, so the page
// shows the correct paper even if scripting never runs; JS then swaps to an
// explicit-slot URL exactly at each slot boundary (DOM swap, never navigation —
// reloads are the watchdog's job) and, on Visionect, parks the panel until the
// next boundary via okular.Sleep. Set the VSS "Automatic page reload" to 0 (or
// leave it as a coarse watchdog): this page paces itself.
var rotationTmpl = template.Must(template.New("rotation").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="43200">
<title>paperboy</title>
<style>
  :root { --pad: 3vmin; }
  html, body { margin: 0; height: 100%; background: #fff; }
  body { box-sizing: border-box; padding: var(--pad); }
  .sheet { display: block; width: 100%; height: 100%; object-fit: contain; background: #fff; }
  body.cover .sheet { object-fit: cover; }
</style>
</head>
<body>
<img id="p" class="sheet" src="{{.InitialSrc}}" alt="">
<script>
(function () {
  // Tiny query parser instead of URLSearchParams: the page must survive the
  // oldest Visionect WebKit builds — if line one throws, there is no rotation
  // and no panel sleep at all.
  function qparam(name) {
    var m = window.location.search.match(new RegExp('[?&]' + name + '=([^&]*)'));
    return m ? decodeURIComponent(m[1].replace(/\+/g, ' ')) : null;
  }
  var margin = qparam('margin');
  if (margin !== null) {
    document.documentElement.style.setProperty('--pad', (parseFloat(margin) || 0) + 'vmin');
  }
  if (qparam('fit') === 'cover') document.body.classList.add('cover');

  var I = {{.IntervalSec}};   // dwell seconds
  var phase = {{.Phase}};
  var sources = qparam('sources');
  var sleepMode = qparam('sleep') || 'auto';
  var img = document.getElementById('p');
  var lastLoadFailed = false;

  function msToBoundary() {
    var t = Date.now() / 1000;
    return Math.max(1000, ((Math.floor(t / I) + 1) * I - t) * 1000 + 500);
  }
  function currentSlot() {
    return Math.floor(Date.now() / 1000 / I) + phase;
  }
  function urlFor(slot) {
    var parts = [];
    if (sources) parts.push('sources=' + encodeURIComponent(sources));
    parts.push('interval=' + I + 's');
    parts.push('slot=' + slot);
    parts.push('w=' + Math.round(window.innerWidth * (window.devicePixelRatio || 1)));
    parts.push('margin=0');
    return '/rotation.png?' + parts.join('&');
  }
  // Visionect only (feature-detected; a no-op in any browser): once the new
  // image has landed, sleep the panel until just past the next boundary — the
  // panel's wake cadence follows the rotation with zero VSS configuration.
  // okular.Sleep dispatches almost immediately, so let the render settle first.
  function scheduleSleep() {
    if (sleepMode === 'off' || lastLoadFailed || !window.okular || !window.okular.Sleep) return;
    setTimeout(function () {
      try { window.okular.Sleep(Math.max(1, Math.ceil(msToBoundary() / 60000))); } catch (e) {}
    }, 1500);
  }
  img.addEventListener('load', function () { lastLoadFailed = false; scheduleSleep(); });
  // A failed swap (cold start, network blip) must not park the panel on a
  // broken frame: keep it awake and retry the current slot shortly.
  img.addEventListener('error', function () {
    lastLoadFailed = true;
    setTimeout(function () { img.src = urlFor(currentSlot()); }, 60000);
  });
  if (img.complete && img.naturalWidth > 0) scheduleSleep();

  function tick() {
    img.src = urlFor(currentSlot());
    setTimeout(tick, msToBoundary());
  }
  setTimeout(tick, msToBoundary());
})();
</script>
</body>
</html>
`))

func handleRotationPage(p *paperboy.Paperboy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		spec, err := parseRotationSpec(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if spec.Slot != nil {
			http.Error(w, "slot pins a single paper and belongs on /rotation.png; the page derives slots from the clock", http.StatusBadRequest)
			return
		}
		// Validate the rotation server-side so a typo'd provisioned URL fails
		// loudly here instead of rendering a permanently broken e-ink frame.
		// A still-filling archive (ErrNoneAvailable) is fine: the page retries.
		if _, err := p.ResolveRotation(spec); errors.Is(err, paperboy.ErrNoSourcesMatch) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		interval := spec.Interval
		if interval <= 0 {
			interval = paperboy.DefaultRotationInterval
		}
		// The initial image is now-resolved (no slot, no width): correct without
		// JS, at master resolution CSS scales down.
		init := url.Values{}
		for _, k := range []string{"sources", "interval", "phase"} {
			if v := r.URL.Query().Get(k); v != "" {
				init.Set(k, v)
			}
		}
		init.Set("margin", "0")

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		// A plain string is correct here: html/template escapes it for the src
		// attribute context (init.Encode() output is already percent-encoded).
		_ = rotationTmpl.Execute(w, map[string]any{
			"InitialSrc":  "/rotation.png?" + init.Encode(),
			"IntervalSec": int(interval / time.Second),
			"Phase":       spec.Phase,
		})
		_ = p // reserved: future server-side embellishment (per-source titles etc.)
	}
}

// handleTRMNLDisplay implements the TRMNL /api/display wire contract over the
// same stateless rotation: the envelope carries a slot-explicit image URL plus
// refresh_rate = seconds to the next slot boundary, so stock TRMNL/BYOS
// firmware wakes exactly when the content changes.
func handleTRMNLDisplay(p *paperboy.Paperboy) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts, err := parseRenderOpts(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		spec, err := parseRotationSpec(r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = opts // parsed for validation only; the params pass through in the image URL

		// Resolve only — no render. The device fetches image_url right after,
		// which is when the (cached) render happens once.
		rot, err := p.ResolveRotation(spec)
		if err != nil {
			writeEngineError(w, err)
			return
		}
		date, ok := p.NewestEdition(rot.SourceID)
		if !ok {
			writeEngineError(w, paperboy.ErrNoneAvailable) // pruned between resolve and here
			return
		}

		img := url.Values{}
		for _, k := range []string{"sources", "interval", "phase", "w", "h", "fit", "margin"} {
			if v := r.URL.Query().Get(k); v != "" {
				img.Set(k, v)
			}
		}
		img.Set("slot", strconv.FormatInt(rot.Slot, 10))

		// Self-hosted deployments front this with their own proxy, so the
		// forwarded scheme/host headers are trusted when present.
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if xf := r.Header.Get("X-Forwarded-Proto"); xf != "" {
			scheme = xf
		}
		host := r.Host
		if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
			host = xfh
		}

		refresh := secondsUntil(rot.NextChange)
		if refresh < 60 {
			refresh = 60 // don't ask firmware to wake more than once a minute
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		// filename is the content identity the firmware uses to skip repaints:
		// source + edition date (+ extension). Deliberately NOT the slot — a
		// one-source rotation must not re-flash the same paper every slot.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"image_url":       scheme + "://" + host + "/rotation.png?" + img.Encode(),
			"filename":        fmt.Sprintf("%s-%s.png", rot.SourceID, date.UTC().Format("20060102")),
			"refresh_rate":    refresh,
			"update_firmware": false,
		})
	}
}
