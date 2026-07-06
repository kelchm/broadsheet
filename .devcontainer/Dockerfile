# Dev container for broadsheet. Includes Go toolchain + all CGo system deps.
# Used by VS Code "Reopen in Container" and `docker compose -f compose.dev.yaml up`.
#
# Mirrors docker/Dockerfile's builder stage; intentionally NOT multi-stage —
# we want the full toolchain available for iteration.

# trixie (not bookworm), matching docker/Dockerfile: go-fitz's prebuilt arm64
# MuPDF lib needs glibc >= 2.38, and this container builds linux/arm64 on
# Apple Silicon hosts.
FROM golang:1.26-trixie

ARG DEBIAN_FRONTEND=noninteractive

# go-fitz links MuPDF statically from a bundled archive via cgo, so we only
# need a C toolchain — no system PDF/vision libraries.
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        git \
        curl \
    && rm -rf /var/lib/apt/lists/*

# golangci-lint (v2 — matches .golangci.yml's `version: "2"` schema)
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
    | sh -s -- -b /usr/local/bin v2.12.2

# Non-root user for devcontainer (matches devcontainer.json remoteUser)
ARG USERNAME=vscode
ARG USER_UID=1000
ARG USER_GID=$USER_UID
RUN groupadd --gid $USER_GID $USERNAME \
    && useradd --uid $USER_UID --gid $USER_GID -m -s /bin/bash $USERNAME

USER $USERNAME
WORKDIR /workspaces/broadsheet

# Pre-warm Go env
ENV GOFLAGS=-buildvcs=false
