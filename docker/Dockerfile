# Production multi-stage build for paperboy-server.
# Final image carries Go-built binary + runtime C libs (OpenCV, MuPDF, Tesseract).

# --- builder ---------------------------------------------------------------
FROM golang:1.26-bookworm AS builder

ARG DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        pkg-config \
        ca-certificates \
        libopencv-dev \
        libmupdf-dev \
        libtesseract-dev \
        libleptonica-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /out/paperboy-server ./cmd/paperboy-server

# --- runtime ---------------------------------------------------------------
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        libopencv-core406 \
        libopencv-imgcodecs406 \
        libopencv-imgproc406 \
        libmupdf-dev \
        libtesseract5 \
        libleptonica-dev \
        tesseract-ocr-eng \
    && rm -rf /var/lib/apt/lists/*

# Non-root runtime user
RUN groupadd --system paperboy && useradd --system --gid paperboy --create-home paperboy
USER paperboy
WORKDIR /home/paperboy

COPY --from=builder /out/paperboy-server /usr/local/bin/paperboy-server

ENV PAPERBOY_PORT=8080 \
    PAPERBOY_DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/paperboy-server"]
