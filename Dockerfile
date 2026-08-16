# syntax=docker/dockerfile:1.7

# ---- Build stage -------------------------------------------------------
# Pinned Go build image. Only this stage ever has the Go toolchain, source
# tree, or build cache — none of that ships in the final image.
FROM golang:1.22-bookworm AS build

WORKDIR /src

# Cache module downloads separately from source changes so `docker build`
# doesn't re-download the module graph on every source edit.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 for a fully static binary — no libc version coupling
# between the build and runtime base images. Trim symbols (-s -w) and
# embed the build version if provided (TEAHUB_VERSION build-arg), surfaced
# by /api/health for deploy verification.
ARG TEAHUB_VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${TEAHUB_VERSION}" \
    -o /out/teahub-server \
    ./backend/cmd/server

# ---- Runtime stage -------------------------------------------------------
# debian-slim rather than distroless: TeaHub's gitclone package shells out
# to a real `git` binary at request time (see ARCHITECTURE.md "Why shell
# out to git"), and git's own dynamic-library footprint (libpcre2, libcurl,
# zlib, openssl) is exactly what a real Debian base already provides —
# reproducing that manually on top of distroless/base risks silently
# missing a shared library in production. This trades a few extra MB of
# image size for an image whose runtime dependencies are guaranteed
# consistent, which is the safer default for a project this size.
FROM debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 teahub \
    && useradd --uid 65532 --gid teahub --no-create-home --shell /usr/sbin/nologin teahub

COPY --from=build /out/teahub-server /usr/local/bin/teahub-server
COPY --chown=teahub:teahub frontend /app/frontend

WORKDIR /app
ENV REPO_STORAGE_DIR="/data/repos" \
    PORT="8080"

# /data is a mount point for a persistent volume in any deployment that
# wants clones to survive a restart (see docker-compose.yml and
# deploy/cloudrun.md). On Cloud Run's default ephemeral disk it's scratch
# space only — the app already tolerates that by re-cloning on demand
# (see gitclone.CloneInto's doc comment).
RUN mkdir -p /data/repos && chown -R teahub:teahub /data
VOLUME ["/data"]

EXPOSE 8080
USER teahub:teahub

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD curl -f "http://localhost:${PORT}/api/health" || exit 1

ENTRYPOINT ["/usr/local/bin/teahub-server"]
