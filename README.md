# paperboy

Fetches newspaper front pages and rotates them on a display.

I built it for a wall-mounted e-ink display — originally a Visionect 13". It runs as a standalone HTTP server, an embeddable Go library, or the backend for a TRMNL plugin.

It's inspired by [newsprint](https://github.com/graiz/newsprint), with a few things that one didn't do:

- Serves from a local archive, so a paper being down doesn't leave you looking at "Newspaper File Not Found".
- Fetches in the background on a schedule whether or not anyone's asking, and keeps a few days of history. Requests just read from disk — nothing's fetched in the request path.
- One engine behind both the server and the library.

There's a longer write-up in [docs/architecture.md](docs/architecture.md).

## Quick start

```sh
git clone git@github.com:kelchm/paperboy.git && cd paperboy

# native (macOS) — fastest dev loop. You just need a C compiler; cgo links the
# MuPDF that ships bundled with go-fitz, and nothing else.
xcode-select --install       # one-time, skip if you already have it
mise install                 # installs the Go version from .mise.toml
make run                     # builds and runs the server on :8080

# or a dev container, if you'd rather not install anything on the host:
docker compose -f compose.dev.yaml up --build
```

Then open <http://localhost:8080/rotation>.

## How it works

Fetching and serving are separate. A background loop mirrors upstream into a local archive, and the HTTP handlers only ever read from that archive — they never fetch. That's the whole idea: requests are fast, and upstream being flaky doesn't land in the request path.

```
cmd/
  paperboy/          CLI (debug: fetch, list, health)
  paperboy-server/   the HTTP server
internal/            not importable from outside
  source/              the core contracts: Source, Provider, Edition
  provider/            upstream drivers (freedomforum/ is the first)
  registry/            the built-in source list
  reconcile/           the background loop: poll -> archive -> prune
  archive/             durable PDF store, keyed by edition date
  render/              archived artifact -> master-width PNG
  rasterize/           PDF -> image (go-fitz / MuPDF)
  store/               paperboy.db (SQLite): sources, ETags, health history
  catalog/             embedded list of known papers (seeds the store)
  cache/               legacy state.json reader (one-time import)
pkg/paperboy/        the public API, for embedding
docker/              production Dockerfile + compose
.devcontainer/       VS Code dev container
```

### The background loop (every `PAPERBOY_POLL_INTERVAL`)

```
for each source:
  provider.Poll()      conditional GET the 3 day-of-month folders that could
                       hold a current edition (UTC yesterday/today/tomorrow)
    304 = unchanged    404 = nothing there    200 = a new edition
  archive anything new (filed under its Last-Modified date)
  prune editions older than PAPERBOY_ARCHIVE_DAYS
```

### Serving a request (no network)

```
GET /rotation.png?sources=ny-nyt,wsj&interval=30m
  slot    = floor(now / interval) + phase     # pure function of the clock
  source  = sources[ slot mod len(sources) ]
       |  (nothing archived for it yet)
       -> next source that has content, with X-Paperboy-Stale: true
       |  (archive is completely empty — cold start only)
       -> 503
  render, frame, respond with ETag + max-age until the next slot
```

Once the archive has anything in it you shouldn't see a "Not Found". And because
rotation is a function of the clock — not a stored cursor — every GET is an
idempotent read: previews, proxies, monitors, and curl can't perturb a display.

## Endpoints

| Endpoint | What it does |
|---|---|
| `GET /rotation` | The display page — fills the viewport, frames the paper in CSS, swaps to the next paper exactly at each slot boundary, and puts a Visionect panel to sleep until then. Point HTML-rendering displays here. |
| `GET /rotation.png` | The rotation's current paper as a raw PNG, with `ETag` and `Cache-Control: max-age=<seconds to next slot>`. For image-pull devices. |
| `GET /api/display` | TRMNL-compatible JSON envelope: `image_url` + `refresh_rate` (seconds until the content next changes). Point TRMNL/BYOS firmware here. |
| `GET /paper/{id}.png` | The newest archived edition for one source. `ETag`'d pure read. |
| `GET /paper/{id}/{date}.png` | A specific archived edition (`YYYYMMDD`). |
| `GET /sources` | JSON: the configured sources and their health. |
| `/api/v1/…` | The management plane: `GET /status`, `GET /sources` (full catalog + enabled flags + health), `PATCH /sources/{id}` (`{"enabled": bool}` — applies live), `POST /sources/{id}/refresh`, `GET /sources/{id}/editions`. Mutations honor `PAPERBOY_ADMIN_TOKEN` when set. |
| `GET /health` | Liveness — 200 as long as the process is up. |
| `GET /healthz` | Readiness — 200 once there's at least one edition archived. |
| `GET /`, `GET /current.png` | **Deprecated** advance-on-GET rotation; use `/rotation` / `/rotation.png`. Removed before 1.0. |

The image endpoints take framing params: `?w=` / `?h=` (target size), `?fit=contain\|cover`, `?margin=<pct>`. The rotation endpoints take `?sources=<ids>` (subset + order), `?interval=<dur>` (dwell per paper, default 30m), `?phase=<n>` (offset a display within the same playlist), `?slot=<n>` (pin an exact slot). Every response carries `X-Paperboy-Source`, `-Width`, `-Height`, `-Days-Old`; rotation responses add `X-Paperboy-Slot` and `X-Paperboy-Next-Change`, plus `X-Paperboy-Stale: true` if an empty source was substituted.

## Displays

All per-display configuration lives in the URL you provision on the device — there are no display records to manage in paperboy. Three ways to drive a screen:

- **It renders HTML** (Visionect is literally an HTML rendering engine; also browsers, dashboards) → point it at `GET /rotation?sources=…&interval=30m`. The page fits any screen with no per-device setup (`?fit=cover`, `?margin=<n>` to tune), advances itself exactly at slot boundaries, and — on Visionect — feature-detects the okular API and sleeps the panel until the next boundary (`?sleep=off` to disable). **Set VSS "Automatic page reload" to 0**; the page paces itself, and a reload timer pointed at a bare image URL will silently skip papers whenever it ticks slower than the interval.
- **It pulls a raw image** (fixed-resolution panels, Home Assistant, …) → point it at `/rotation.png?sources=…&interval=30m&w=<W>&h=<H>`. Fetch at least as often as the interval; the `ETag`/`304` makes redundant fetches nearly free, and `X-Paperboy-Next-Change` says exactly how long to sleep.
- **It speaks TRMNL** → point it at `/api/display?sources=…&interval=30m&w=800&h=480`. The envelope's `refresh_rate` wakes the firmware at the next slot boundary (clamped to ≥60s). Caveat: the image is a grayscale PNG — fine for BYOS/custom clients and anything that renders PNGs, but **stock TRMNL firmware expects a 1-bit image**; that output format lands with the per-device dithering work.

Displays sharing a playlist stay in sync automatically; give one `?phase=1` to deliberately show the next paper over. `/paper/{id}` never rotates anything — it's always just that paper.

## Sizing

The client picks the size, not the server. One instance might feed a 13" Visionect, a TRMNL, a browser tab, and a Home Assistant card, and they all want different widths — so it's a per-request thing.

- `PAPERBOY_WIDTH` (default 1600) is the *master* width: what we rasterize and cache at. Treat it as the quality ceiling.
- `?w=<int>` resizes down from the master, per request. Aspect ratio's kept; height follows.
- Ask for more than the master and you just get the master back — upscaling only softens the text.

```sh
curl http://localhost:8080/rotation.png           # master width (1600px)
curl http://localhost:8080/rotation.png?w=800     # 800px wide, height proportional
curl http://localhost:8080/paper/ny-nyt.png?w=480 # a specific source, 480px wide
```

## Embedding

```go
import "github.com/kelchm/paperboy/pkg/paperboy"

p, _ := paperboy.New(paperboy.Config{DataDir: "./data"})

// Kick off the background mirror so the archive stays current. Without this the
// engine is passive — it only serves what's already on disk.
p.StartReconciler(ctx)

res, err := p.RenderCurrent(ctx)                                           // master width
res, err := p.RenderCurrent(ctx, paperboy.RenderOptions{OutputWidth: 800}) // resized
// res.Image is PNG bytes; res.Width / res.Height are the actual dimensions
```

## Configuration

Everything's an env var:

| Var | Default | What it does |
|---|---|---|
| `PAPERBOY_PORT` | `8080` | HTTP port |
| `PAPERBOY_DATA_DIR` | `./data` | Holds `archive/` (PDFs), `cache/` (PNGs), and `paperboy.db` |
| `PAPERBOY_WIDTH` | `1600` | Master width — what we cache at. `?w=` resizes down from here. |
| `PAPERBOY_POLL_INTERVAL` | `30m` | How often the background loop checks upstream |
| `PAPERBOY_ARCHIVE_DAYS` | `14` | How many days of editions to keep |
| `PAPERBOY_ADMIN_TOKEN` | *(unset)* | When set, mutating `/api/v1` calls require `Authorization: Bearer <token>`. Set it before exposing the server beyond a trusted network. |
| `PAPERBOY_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

## Sources

Front pages come from [freedomforum.org](https://www.freedomforum.org/todaysfrontpages/)'s daily archive. The built-in list lives in [`internal/registry/registry.go`](internal/registry/registry.go) — a paper is a one-liner binding an ID to a provider (`FreedomForum{Prefix: …}`), where the prefix is the code from the Freedom Forum URL. A source on some other site gets a new [provider](internal/provider/).

Worth knowing: Freedom Forum only keeps about two days live, so the archive fills in over time as paperboy runs. It can't go back and grab history that's already rolled off upstream.

## Not done yet

Smart crop. Right now a front page is served whole, exactly as the PDF rasterizes. The plan is to detect each paper's masthead and content edges and frame it automatically. There's a per-source hint field (`CropHints`) carried through for it, but the detector that would use it isn't written.

## License

MIT — see [LICENSE](LICENSE).
