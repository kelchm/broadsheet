# trixie (not bookworm): go-fitz's prebuilt arm64 MuPDF lib needs glibc >= 2.38.
FROM golang:1.26-trixie AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG REVISION=none
ARG BUILD_DATE=unknown

ENV CGO_ENABLED=1
RUN go build -trimpath \
    -ldflags="-s -w \
      -X github.com/kelchm/broadsheet/internal/buildinfo.Version=${VERSION} \
      -X github.com/kelchm/broadsheet/internal/buildinfo.Commit=${REVISION} \
      -X github.com/kelchm/broadsheet/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/broadsheet-server ./cmd/broadsheet-server
RUN mkdir -p /data

FROM gcr.io/distroless/cc-debian13:nonroot
COPY --from=builder /out/broadsheet-server /usr/local/bin/broadsheet-server
COPY --from=builder --chown=nonroot:nonroot /data /data
# Soft heap ceiling: keeps render spikes GC-bounded on small hosts. Override
# (or unset with GOMEMLIMIT=off... actually set higher) for large instances.
ENV GOMEMLIMIT=256MiB \
    BROADSHEET_PORT=8080 \
    BROADSHEET_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/usr/local/bin/broadsheet-server"]
