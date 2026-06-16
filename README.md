# paperboy

Fetches newspaper front pages and rotates them on a display.

Built for wall-mounted e-ink displays (the original use case is a Visionect 13" screen). Works as a self-contained HTTP server, an embeddable Go library, or the backing service for a TRMNL plugin.

This is a Go rewrite of [newsprint](https://github.com/kelchm/newsprint), with three goals beyond the original:

- **Graceful failure.** When a feed is down, fall back across sources instead of showing "Newspaper File Not Found".
- **Smart crop.** Detect the masthead and content boundary automatically instead of hand-tuned per-source CSS offsets.
- **Library + server split.** The same engine powers the standalone HTTP service and embedded use cases.

## Quick start

```sh
# clone
git clone git@github.com:kelchm/paperboy.git && cd paperboy

# option A: native (macOS) — fastest local dev loop
brew install opencv mupdf tesseract pkg-config
mise install                 # picks up .mise.toml; installs Go
make run                     # builds and runs the server on :8080

# option B: dev container — zero host installs (cross-platform)
# VS Code: "Reopen in Container"
# Or:
docker compose -f compose.dev.yaml up --build
```

Open <http://localhost:8080/current.png>.

## Architecture

```
cmd/
  paperboy/          CLI binary (debug: fetch, list, crop preview)
  paperboy-server/   HTTP server binary
internal/            implementation, not importable externally
  source/              source registry (NYT, WP, etc.)
  fetch/               PDF fetcher
  rasterize/           PDF → image
  crop/                smart alignment / masthead detection
  cache/               filesystem images + atomic JSON state
  rotation/            pick-next + cross-source graceful fallback
pkg/paperboy/        public API for embedded use
docker/              production Dockerfile + compose
.devcontainer/       VS Code dev container definition
```

### Request flow

```
GET /current.png
  → rotation.PickNext()                  → sourceId
  → fetch + rasterize + crop             → image
       ↓ (404 from feed)
       try yesterday, then 2 days ago
       ↓ (all dates fail for this source)
       mark source unhealthy, advance rotation, try the next source
       ↓ (all sources fail)
       serve the most recently cached image from any source with a staleness header
```

You should never see "Not Found." That's the bug fix.

## HTTP endpoints

| Endpoint | Description |
|---|---|
| `GET /current.png` | The current rotation slot, advancing each call |
| `GET /next.png` | Force-advance and return the next image |
| `GET /paper/{id}.png` | Render a specific source by id |
| `GET /sources` | JSON list of configured sources + health |
| `GET /health` | Liveness probe (always 200 if process is up) |
| `GET /healthz` | Readiness probe (200 only when at least one source has a usable image) |

## Embedding as a library

```go
import "github.com/kelchm/paperboy/pkg/paperboy"

p, _ := paperboy.New(paperboy.Config{...})
img, meta, err := p.RenderNext(ctx)
```

## Configuration

All config is via environment variables (validated at startup):

| Var | Default | Description |
|---|---|---|
| `PAPERBOY_PORT` | `8080` | HTTP listen port |
| `PAPERBOY_DATA_DIR` | `./data` | Where cached images and state.json live |
| `PAPERBOY_WIDTH` | `1600` | Target image width in pixels |
| `PAPERBOY_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `PAPERBOY_CROP_OCR` | `false` | Enable optional OCR-based masthead refinement |

## Sources

Newspaper front pages come from [freedomforum.org](https://www.freedomforum.org/todaysfrontpages/)'s daily archive. Sources are declared in [`internal/source/registry.go`](internal/source/registry.go). Adding a new paper is a one-line entry; the prefix comes from the Freedom Forum URL.

## License

MIT — see [LICENSE](LICENSE).
