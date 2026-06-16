# Dev container for paperboy. Includes Go toolchain + all CGo system deps.
# Used by VS Code "Reopen in Container" and `docker compose -f compose.dev.yaml up`.
#
# Mirrors docker/Dockerfile's builder stage; intentionally NOT multi-stage —
# we want the full toolchain available for iteration.

FROM golang:1.26-bookworm

ARG DEBIAN_FRONTEND=noninteractive

# CGo system deps: OpenCV (gocv), MuPDF (go-fitz), Tesseract (gosseract)
# plus general build tools.
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        pkg-config \
        ca-certificates \
        git \
        curl \
        libopencv-dev \
        libmupdf-dev \
        mupdf-tools \
        libtesseract-dev \
        libleptonica-dev \
        tesseract-ocr \
        tesseract-ocr-eng \
    && rm -rf /var/lib/apt/lists/*

# golangci-lint
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
    | sh -s -- -b /usr/local/bin v1.62.2

# Non-root user for devcontainer (matches devcontainer.json remoteUser)
ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=$USER_UID
RUN groupadd --gid $USER_GID $USERNAME \
    && useradd --uid $USER_UID --gid $USER_GID -m -s /bin/bash $USERNAME

USER $USERNAME
WORKDIR /workspaces/paperboy

# Pre-warm Go env
ENV GOFLAGS=-buildvcs=false
