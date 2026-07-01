# Production build for paperboy-server.
#
# go-fitz links MuPDF statically from a bundled archive via cgo, so the build
# needs a C compiler (already present in the golang image) but NO system PDF or
# vision libraries. The resulting binary still dynamically links glibc, so the
# runtime is distroless/cc (glibc + libgcc + ca-certificates) — not scratch.

# --- builder ---------------------------------------------------------------
FROM golang:1.26-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build metadata stamped into internal/buildinfo via ldflags. Defaults keep an
# un-stamped local `go build` honest; CI passes the real version/commit/date.
ARG VERSION=dev
ARG REVISION=none
ARG BUILD_DATE=unknown

# CGO is required (and on by default) so go-fitz links the bundled MuPDF.
ENV CGO_ENABLED=1
RUN go build -trimpath \
    -ldflags="-s -w \
      -X github.com/kelchm/paperboy/internal/buildinfo.Version=${VERSION} \
      -X github.com/kelchm/paperboy/internal/buildinfo.Commit=${REVISION} \
      -X github.com/kelchm/paperboy/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/paperboy-server ./cmd/paperboy-server

# Pre-create the data dir owned by the nonroot runtime uid so a mounted volume
# inherits writable ownership on first use.
RUN mkdir -p /data

# --- runtime ---------------------------------------------------------------
FROM gcr.io/distroless/cc-debian12:nonroot

COPY --from=builder /out/paperboy-server /usr/local/bin/paperboy-server
COPY --from=builder --chown=nonroot:nonroot /data /data

ENV PAPERBOY_PORT=8080 \
    PAPERBOY_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080

USER nonroot
ENTRYPOINT ["/usr/local/bin/paperboy-server"]
