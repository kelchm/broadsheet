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
      -X github.com/kelchm/paperboy/internal/buildinfo.Version=${VERSION} \
      -X github.com/kelchm/paperboy/internal/buildinfo.Commit=${REVISION} \
      -X github.com/kelchm/paperboy/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/paperboy-server ./cmd/paperboy-server
RUN mkdir -p /data

FROM gcr.io/distroless/cc-debian13:nonroot
COPY --from=builder /out/paperboy-server /usr/local/bin/paperboy-server
COPY --from=builder --chown=nonroot:nonroot /data /data
ENV PAPERBOY_PORT=8080 \
    PAPERBOY_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/usr/local/bin/paperboy-server"]
