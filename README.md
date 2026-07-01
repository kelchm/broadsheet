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

Then open <http://localhost:8080/current.png>.

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
  cache/               small JSON state file (per-source health + ETags)
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
GET /current.png
  slot    = (now / PAPERBOY_ROTATE_INTERVAL) mod number-of-sources
  edition = newest archived edition for that source
       |  (nothing archived for it yet)
       -> newest edition from any source, with X-Paperboy-Stale: true
       |  (archive is completely empty — cold start only)
       -> 503
  render, resize, done
```

Once the archive has anything in it you shouldn't see a "Not Found". That was the point of the rewrite.

## Endpoints

| Endpoint | What it does |
|---|---|
| `GET /current.png` | The current rotation slot. It's time-based, so hitting it repeatedly is a safe read — it doesn't advance. |
| `GET /paper/{id}.png` | The newest archived edition for one source. |
| `GET /sources` | JSON: the configured sources and their health. |
| `GET /health` | Liveness — 200 as long as the process is up. |
| `GET /healthz` | Readiness — 200 once there's at least one edition archived. |

Image endpoints take `?w=<int>` for the output width (see [Sizing](#sizing)). Every response carries `X-Paperboy-Source`, `-Width`, `-Height`, and `-Days-Old`, plus `X-Paperboy-Stale: true` if the slot's source had nothing and you got a fallback from another one.

## Sizing

The client picks the size, not the server. One instance might feed a 13" Visionect, a TRMNL, a browser tab, and a Home Assistant card, and they all want different widths — so it's a per-request thing.

- `PAPERBOY_WIDTH` (default 1600) is the *master* width: what we rasterize and cache at. Treat it as the quality ceiling.
- `?w=<int>` resizes down from the master, per request. Aspect ratio's kept; height follows.
- Ask for more than the master and you just get the master back — upscaling only softens the text.

```sh
curl http://localhost:8080/current.png            # master width (1600px)
curl http://localhost:8080/current.png?w=800      # 800px wide, height proportional
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
| `PAPERBOY_DATA_DIR` | `./data` | Holds `archive/` (PDFs), `cache/` (PNGs), and `state.json` |
| `PAPERBOY_WIDTH` | `1600` | Master width — what we cache at. `?w=` resizes down from here. |
| `PAPERBOY_POLL_INTERVAL` | `30m` | How often the background loop checks upstream |
| `PAPERBOY_ROTATE_INTERVAL` | `1h` | How long each source stays the `/current.png` slot |
| `PAPERBOY_ARCHIVE_DAYS` | `14` | How many days of editions to keep |
| `PAPERBOY_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

## Sources

Front pages come from [freedomforum.org](https://www.freedomforum.org/todaysfrontpages/)'s daily archive. The built-in list lives in [`internal/registry/registry.go`](internal/registry/registry.go) — a paper is a one-liner binding an ID to a provider (`FreedomForum{Prefix: …}`), where the prefix is the code from the Freedom Forum URL. A source on some other site gets a new [provider](internal/provider/).

Worth knowing: Freedom Forum only keeps about two days live, so the archive fills in over time as paperboy runs. It can't go back and grab history that's already rolled off upstream.

## Not done yet

Smart crop. Right now a front page is served whole, exactly as the PDF rasterizes. The plan is to detect each paper's masthead and content edges and frame it automatically. There's a per-source hint field (`CropHints`) carried through for it, but the detector that would use it isn't written.

## License

MIT — see [LICENSE](LICENSE).
