# paperboy architecture

How paperboy works. Written alongside the 0.0.1 rework — the git history has the
blow-by-blow if you want it.

## The core idea

Upstream isn't a smart API. freedomforum is basically a dumb CDN serving static
PDFs at predictable URLs. So the useful way to think about paperboy is as a
mirror: a background loop copies front pages into a local archive as they show
up, and the HTTP side only ever reads from that archive.

Everything else hangs off one decision — fetching and serving are separate. An
HTTP request never triggers a fetch. The reconciler fills the archive; the
handlers just read local disk.

That's what keeps the server fast (no fetch or render in the request path) and
keeps upstream flakiness out of the request path entirely. Rotation is a pure
function of the wall clock (see [rotation](#rotation)), so handlers really are
pure reads — nothing mutates on GET. (The deprecated `/current.png` cursor path
is the one legacy exception until it's removed.)

```
                 ┌─────────────────┐        ┌──────────────┐
  freedomforum ─▶│   reconciler    │ writes │  PDF archive │
   (static CDN)  │ (background,    │───────▶│  (durable)   │
                 │  every ~30 min) │        └──────┬───────┘
                 └─────────────────┘               │ render (lazy)
                                                    ▼
                 ┌─────────────────┐  reads  ┌──────────────┐
  HTTP clients ─▶│  handlers       │◀────────│  PNG cache   │
  (displays)     │ (pure reads)    │         │ (disposable) │
                 └─────────────────┘         └──────────────┘
```

## Storage layout

```
<PAPERBOY_DATA_DIR>/
  archive/<source-id>/<YYYYMMDD>.pdf   # durable source of truth; kept N days
  cache/<source-id>/<YYYYMMDD>.w<W>.png # disposable master renders (keyed by
                                        # edition date + master width); evict anytime
  state.json                           # per-source health + provider ETags
```

Two directories, very different lifetimes:

- `archive/` — the PDFs are the state. They're what has value, what gets a
  retention policy, and what edition dates are keyed on. Written atomically
  (temp + rename).
- `cache/` — the PNGs aren't state. They're just renders derived from a PDF, with
  the archive's retention applied to them (aged-out renders are pruned each
  cycle). Evict any PNG whenever you like and you've lost a few hundred
  milliseconds of re-render, nothing more. `rm -rf cache/` is always safe.

`state.json` is in the same spirit: it holds only re-derivable bookkeeping
(health timestamps + provider ETags), so a corrupt file is set aside as
`state.json.corrupt` and the server starts fresh rather than refusing to boot.

Everything's keyed by edition date (`YYYYMMDD`, from the PDF's `Last-Modified`
header, falling back to the probed folder's date if that header is missing),
not by the day-of-month we happened to request — see [trusting the
origin](#trusting-the-origin).

## Providers

A Provider is the seam that keeps one upstream's quirks out of everything else.
All the freedomforum-specific stuff — the day-of-month URLs, the 3-folder probe
window, conditional GET / ETags, "the date comes from `Last-Modified`" — lives
inside a provider. The engine never sees any of it.

```go
// MediaType is the kind of artifact a provider returns.
type MediaType string
const (
    MediaPDF   MediaType = "application/pdf"
    MediaImage MediaType = "image/png" // already-rendered sources skip rasterizing
)

// Edition is one fetched newspaper edition.
type Edition struct {
    Date    time.Time // edition date (FreedomForum: from Last-Modified)
    Version string    // opaque change token (FreedomForum: ETag) — the engine persists it
    Media   MediaType // whether we rasterize or use the bytes directly
    Data    []byte
}

// Deps are shared runtime deps injected per-poll, so Provider *values* stay pure
// config (many sources, one shared HTTP connection).
type Deps struct {
    HTTP   *http.Client
    Logger *slog.Logger
}

// Provider acquires editions for the sources it backs.
type Provider interface {
    // Poll returns editions new or changed since `seen` (the tokens we persisted
    // last time), plus the tokens to persist for next time. It may return none.
    Poll(ctx context.Context, deps Deps, seen map[string]string, now time.Time) (
        editions []Edition, versions map[string]string, err error)
}
```

The version map is opaque to the engine — it stores whatever the provider hands
back and returns it on the next poll. That keeps all the change-detection
bookkeeping (ETags, feed cursors, API pagination, whatever) inside the provider.

### Sources are typed

A `Source` is a paper we serve. Its `Provider` is a typed value that's both the
per-source config and the behavior:

```go
type Source struct {
    ID          string
    DisplayName string
    CropHints   CropHints
    Provider    Provider // typed config + behavior
}

// FreedomForum backs sources on freedomforum.org's daily archive.
type FreedomForum struct {
    Prefix string // e.g. "NY_NYT"
}

func (f FreedomForum) Poll(ctx context.Context, deps Deps, seen map[string]string, now time.Time) (
    []Edition, map[string]string, error) {
    // 1. compute the 3 day-of-month URLs from `now` (UTC yesterday/today/tomorrow)
    // 2. conditional GET each with seen[url] as If-None-Match
    //      304 -> unchanged, skip
    //      404 -> nothing there, skip
    //      200 -> Edition{Date: lastModified, Version: etag, Media: MediaPDF, Data: body}
    // 3. return editions + updated versions
}
```

Registry entries stay one-liners, just typed:

```go
{ID: "ny-nyt", DisplayName: "The New York Times",
 Provider:  FreedomForum{Prefix: "NY_NYT"},
 CropHints: CropHints{MastheadText: "The New York Times"}},
```

The freedomforum-specific `Prefix` lives on the provider now, instead of being
smeared across the generic `Source`.

The tradeoff, which I'm fine with at this stage: a `Source` holds behavior (an
interface value), so sources are Go-defined for now — you can't load them
straight from a YAML/JSON file without a registry or decoder. If config-file
sources ever become a goal, that's exactly where a params-decoding provider (or a
`ConfigurableProvider[C]`) would slot in, without touching this interface.

### Adding a provider

Implement `Provider` in `internal/provider/<name>` and return `Edition`s with the
right `Media`. Nothing else changes — the engine archives, renders (per `Media`),
prunes, and serves the same way regardless of provider.

## The reconciler

A background loop the server starts (the library doesn't — see [library vs
server](#library-vs-server)). On boot, then every `PAPERBOY_POLL_INTERVAL`, for
each source:

```
seen := state.versions[source.ID]
editions, versions := source.Provider.Poll(deps, seen, now)
for each edition: archive.Put(source.ID, edition)   # atomic; keyed by edition date
persist versions -> state.versions[source.ID]   # minus tokens of editions that failed to store
record health: success if we stored anything; failure on poll or store errors
prune archive/<id>/* older than PAPERBOY_ARCHIVE_DAYS
```

Right now sources are polled sequentially on a fixed interval, and a source that
errors just gets retried next cycle. That's already gentle on the CDN (see
below), so jitter, backoff, and bounded concurrency are refinements I've left for
later rather than built.

### Why three probes, and why it's cheap

The provider probes three day-of-month folders — UTC yesterday, today, tomorrow.
That's the smallest window that works for any timezone: the earth spans UTC−12 to
UTC+14, so a paper's local date is always within a day of UTC's. Three folders
therefore always cover the one holding its current edition, without paperboy
knowing anything about that paper's timezone or press schedule. It's a couple of
cheap requests instead of a per-paper schedule table nobody wants to maintain.

It stays cheap because the probes are conditional. With a stored ETag an
unchanged folder is a `304` (no body) and a missing one is a `404` (no body), so
a full PDF only crosses the wire when there's genuinely a new edition. For 6
sources on a 30-minute cadence that's ~18 tiny requests a cycle (~600/day),
nearly all 304/404, plus a handful of real downloads. It's all one host over
HTTP/2, so a single connection multiplexes everything, and the User-Agent names
the project so the CDN operator can find us.

We never try to decide whether a day's edition is "final" — we can't know that,
and we don't need to. We re-probe the window every cycle; a `304` just means "do
nothing," whether the edition is done forever or simply hasn't changed yet. A
folder drops out of the rotation only when it slides past the yesterday/today/
tomorrow window as the date rolls forward.

## Rendering

Rendering turns an archived artifact into a master-width grayscale PNG in
`cache/`:

- `MediaPDF` — rasterize page 1 (go-fitz / MuPDF) at a DPI derived from the
  page bounds (~2x the master width, clamped to 300 — a fixed 300 DPI was a
  ~100MB+ allocation per broadsheet), grayscale, resize to the master width,
  write atomically.
- `MediaImage` — decode, grayscale, resize, write.

It's lazy: a PNG is produced the first time someone asks for that (source,
date, master width) and cached after. Freshness is checked against the archived
artifact's mtime, so a re-posted (corrected) edition re-renders instead of
serving the stale PNG, and concurrent cold-cache requests for the same PNG are
collapsed into a single render (singleflight). The cache is pruned on the same
retention as the archive. Pre-rendering in the reconciler is a possible
optimization later — it'd make every request instant at the cost of rendering
papers nobody looks at — but it wouldn't change the model (the PDF archive is
still the source of truth), so it can be added whenever without disruption.

Per-request `?w=` resizes down from the cached master (see [sizing](#sizing));
those per-width outputs are computed per request, not stored.

## HTTP API

Every handler is a pure read over the local archive/cache. None of them fetch.

| Endpoint | Behavior |
|---|---|
| `GET /rotation` | Display page for HTML renderers (Visionect, browsers) — self-pacing: swaps to the next paper exactly at each slot boundary and manages Visionect panel sleep. |
| `GET /rotation.png` | The rotation's current paper as a raw PNG. Pure read; `ETag` + `Cache-Control: max-age=<to boundary>`. |
| `GET /api/display` | TRMNL-wire-compatible JSON envelope: `image_url` (slot-explicit) + `refresh_rate` (seconds to the next slot boundary). |
| `GET /paper/{id}.png` | Newest archived edition for a specific source. Pure read, `ETag`'d. |
| `GET /sources` | JSON: the configured sources and their health. |
| `GET /health` | Liveness — 200 whenever the process is up. |
| `GET /healthz` | Readiness — 200 once at least one usable edition is archived. |
| `GET /`, `GET /current.png` | **Deprecated.** The old advance-on-GET rotation (per-device cursor, `?device=`). Kept for existing deployments; will be removed before 1.0. |

`GET /paper/{id}/{date}.png` (a specific archived edition) is an obvious future
addition now that there's a real archive, but it isn't there yet.

The image endpoints take framing params — `?w=` / `?h=` (target size),
`?fit=contain|cover`, `?margin=<pct>` — and the rotation endpoints take
`?sources=<ids>`, `?interval=<dur>`, `?phase=<n>`, `?slot=<n>`. Every response
sets `X-Paperboy-Source`, `-Width`, `-Height`, and `-Days-Old`, plus
`X-Paperboy-Stale: true` when a slot's source had nothing archived and the next
source with content was substituted. Rotation responses add `X-Paperboy-Slot`
and `X-Paperboy-Next-Change` (seconds until the rotation advances).

`X-Paperboy-Days-Old` is `floor(now − edition date)` in whole days — elapsed time
since the edition, which sidesteps the timezone off-by-one you'd get comparing
against a wall-clock "today."

### Rotation

Which paper a rotation shows is a pure function of the wall clock and the URL —
no cursor, no per-device state, nothing mutates on read:

```
slot  = floor(now / interval) + phase      (or an explicit ?slot=N)
index = slot mod len(sources)
```

All per-display configuration travels in the URL the operator provisions on the
device (there is deliberately no Display resource to manage). Because every GET
is idempotent, previews, prefetchers, proxies, monitors, and debugging curls are
all harmless; displays sharing a playlist stay in sync automatically (or
deliberately offset via `phase=`); and a restart changes nothing. If the slot's
source has nothing archived yet (cold-start fill-in), the rotation
deterministically advances to the next source that does and marks the response
`X-Paperboy-Stale` — every client makes the same substitution.

The slot boundary doubles as the refresh hint — "when does this content next
change" — delivered in whatever transport each device class understands:

- **Raw pullers:** `Cache-Control: max-age=<to boundary>` + `ETag` (skip the
  repaint on 304) + `X-Paperboy-Next-Change`.
- **TRMNL/BYOS:** the envelope's `refresh_rate` (clamped ≥60s), so the firmware
  wakes at the boundary. The image is a grayscale PNG: wire-compatible today for
  BYOS/custom clients; stock TRMNL firmware additionally needs the 1-bit output
  format that lands with the per-device dithering work.
- **Visionect:** the display page itself. VSS runs a persistent server-side
  browser, so the page is the clock: it server-renders the current slot's image
  (correct even if JS never runs), swaps to an explicit-slot URL
  (`/rotation.png?…&slot=N`) exactly at each boundary via DOM swap, keeps a 12h
  meta-refresh watchdog, and — feature-detecting `window.okular` — parks the
  panel with `okular.Sleep(minutes-to-boundary)` after each swap lands
  (`?sleep=off` disables). Set the VSS "Automatic page reload" to 0; the page
  paces itself. Never point VSS's reload timer at the bare PNG: a reload period
  slower than the interval silently skips papers.

The deprecated `/current.png` path still advances a per-device in-memory cursor
per fetch (`?device=` else client IP); see git history for that design's
rationale and why it lost — non-idempotent GETs break caching, previews perturb
displays, and IP identity collapses behind a proxy.

### Sizing and framing

The client controls size and framing; the server only decides which image. Two
paths, by device type:

- HTML displays hit `GET /rotation` — the page fills the viewport and frames the
  front page in CSS (`object-fit: contain`, white background, a padding margin).
  No per-device config; the browser already knows its own size.
- Raw-image devices hit `/rotation.png?w=&h=&fit=&margin=` — the server frames
  to exactly that canvas (contain or cover, with a margin), for panels that
  can't do CSS. `fit=cover` never upscales past the master; oversized canvases
  get the native-resolution center region instead.

`PAPERBOY_WIDTH` is the master width we render and cache at (the quality ceiling);
everything downscales from it, never up. One master render feeds a 13" Visionect,
a TRMNL, a Home Assistant card, and a browser tab.

## Configuration

| Var | Default | Description |
|---|---|---|
| `PAPERBOY_PORT` | `8080` | HTTP listen port |
| `PAPERBOY_DATA_DIR` | `./data` | Root for `archive/`, `cache/`, `state.json` |
| `PAPERBOY_WIDTH` | `1600` | Master render width (quality ceiling) |
| `PAPERBOY_POLL_INTERVAL` | `30m` | Reconciler cadence |
| `PAPERBOY_ARCHIVE_DAYS` | `14` | PDF archive retention |
| `PAPERBOY_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

## Why it's built this way

A few decisions worth writing down so we don't re-litigate them:

- Trust the origin on dates. freedomforum serves the genuine current paper for a
  day or `404`s — it doesn't serve stale month-old content (checked: folders
  older than ~2 days `404` instead of returning a prior month's file). So we
  don't fingerprint content, parse per-paper PDF metadata, or otherwise "verify"
  a date the origin already stands behind. We take the date from `Last-Modified`
  and move on.
- <a name="trusting-the-origin"></a>Key by edition date, not the requested day.
  The URL only carries a day-of-month; the response (`Last-Modified`) carries the
  real date. Keying the archive on `Last-Modified` means a paper is always filed
  under its true edition date no matter which folder we probed.
- The archive builds up; it can't be backfilled. freedomforum only keeps ~2 days
  live, so a deep archive exists only because the reconciler fetches daily and
  retains. A fresh install is ~2 days deep whatever `PAPERBOY_ARCHIVE_DAYS` says,
  and fills in over time. That's the main reason to fetch eagerly in the
  background — a lazy, request-driven design can never build history.
- PNGs are cache, not state. See [storage layout](#storage-layout).
- Time-driven rotation, config in the URL. Which paper shows is a pure function
  of the clock and the provisioned URL, so every request is an idempotent read
  (cacheable, preview-safe, restart-proof), displays sharing a playlist stay in
  sync, and the slot boundary doubles as the refresh hint every device class
  gets in its own transport (max-age/ETag, TRMNL refresh_rate, okular.Sleep).
  There is deliberately no server-side Display resource to manage — Visionect
  panels already have their own server, and everything else takes a URL.
- Both health endpoints stay. paperboy should run under compose *or* k8s, and the
  liveness/readiness split costs compose nothing while being load-bearing under an
  orchestrator.
- `?w=` stays. One instance really does serve several device classes with
  different pixel budgets, so render-once / slice-many earns its keep.

## Library vs server

`pkg/paperboy` is a passive engine: it renders and reads. The background
reconciler is a server concern, started explicitly with `p.StartReconciler(ctx)`,
so embedding the engine never quietly spawns a goroutine fetching PDFs. Embedders
that want the mirror opt into it.

## Package layout

```
internal/
  source/            Source, Provider, Edition, Deps, MediaType (contracts; leaf pkg)
  provider/
    freedomforum/    the FreedomForum provider (imports source)
  registry/          the built-in source list (imports source + providers)
  reconcile/         the background loop: poll -> archive -> prune
  archive/           durable PDF store: atomic Put, Newest, prune
  render/            MediaType-aware "normalize to master PNG" (wraps rasterize)
  rasterize/         PDF -> image (go-fitz / MuPDF)
  cache/             state.json: per-source health + provider ETags
  buildinfo/         version string + User-Agent
pkg/paperboy/        the engine: wires it all together; RenderCurrent / RenderFor /
                     StartReconciler
```

Import direction: `source` is a leaf (just contracts); providers, the registry,
and the engine depend on it, never the other way. The registry references the
concrete `FreedomForum` type, so it lives in its own package rather than in
`source` — that keeps `source` free of provider implementations.
