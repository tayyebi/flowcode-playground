# syntax=docker/dockerfile:1

# FlowCode Playground.
#
# Four stages, in dependency order: the C toolchain builds flowcode itself and
# our tracing driver, Node builds the frontend, Go builds the server, and a
# small Alpine image carries the results.
#
# Alpine (musl) throughout on purpose: the C binaries and the runtime image must
# agree on a libc, and building on Debian while running on Alpine is the classic
# way to get a container that starts and then fails to exec anything.

# ---------------------------------------------------------------------------
# Stage 1 — FlowCode compiler, runtime, and the playground's trace driver
# ---------------------------------------------------------------------------
FROM alpine:3.21 AS flowcode-build

RUN apk add --no-cache build-base git

ARG FLOWCODE_REPO=https://github.com/tayyebi/flowcode.git
# Pin this to a tag or commit SHA for a reproducible image. `main` is the
# convenient default, not the reproducible one.
ARG FLOWCODE_REF=main

WORKDIR /build
RUN git clone --depth 1 --branch "${FLOWCODE_REF}" "${FLOWCODE_REPO}" flowcode \
    || (git clone "${FLOWCODE_REPO}" flowcode && git -C flowcode checkout "${FLOWCODE_REF}")

WORKDIR /build/flowcode
# Builds fcc and flowcode. The optional `os` plugin — which does real shell
# execution and network I/O — is deliberately NOT built: `make all` excludes it,
# the CLI has no flag to load a plugin, and the playground must not be the thing
# that changes that.
RUN make

COPY runner/fcplay.c /build/runner/fcplay.c

# The source list is globbed rather than copied from the Makefile's SRC
# variable, so a file added upstream doesn't silently break this build.
# cli.c and compiler.c are excluded because each defines its own main().
RUN set -eux; \
    SOURCES="$(ls src/*.c | grep -Ev '/(cli|compiler)\.c$')"; \
    cc -std=c11 -Wall -Wextra -Werror -pedantic -O2 -Iinclude \
       -o /build/fcplay $SOURCES /build/runner/fcplay.c -ldl

# Fail the build here rather than shipping an image whose Run button is silent.
RUN set -eux; \
    ./fcc samples/hello-world/hello.fc /tmp/hello.fcb; \
    /build/fcplay /tmp/hello.fcb 2>&1 | grep -q 'vm completed successfully'

# ---------------------------------------------------------------------------
# Stage 2 — frontend
# ---------------------------------------------------------------------------
FROM node:22-alpine AS web-build

WORKDIR /app
# Copy the manifests first so `npm ci` is cached independently of source edits.
COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
RUN npm run build

# ---------------------------------------------------------------------------
# Stage 3 — server
# ---------------------------------------------------------------------------
FROM golang:1.23-alpine AS server-build

WORKDIR /src
COPY server/go.mod ./
RUN go mod download

COPY server/ ./
# Static binary: nothing in the server needs cgo, and a dynamic one would tie
# the runtime image's libc to the builder's.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/playground .

# ---------------------------------------------------------------------------
# Stage 4 — runtime
# ---------------------------------------------------------------------------
FROM alpine:3.21

# wget is busybox's, already present; it backs the compose healthcheck.
#
# The uid and gid are pinned rather than left to adduser, because compose has to
# name them when it mounts a tmpfs over /run/play: that mount replaces this
# directory wholesale, root-owned, and the server could not write to it.
RUN addgroup -S -g 10001 play \
    && adduser -S -D -H -u 10001 -G play play

COPY --from=flowcode-build /build/flowcode/fcc      /usr/local/bin/fcc
COPY --from=flowcode-build /build/flowcode/flowcode /usr/local/bin/flowcode
COPY --from=flowcode-build /build/fcplay            /usr/local/bin/fcplay
COPY --from=flowcode-build /build/flowcode/samples  /opt/flowcode/samples
COPY --from=web-build      /app/dist                /srv/web
COPY --from=server-build   /out/playground          /usr/local/bin/playground

# /run/play is where each submission gets its own scratch directory. Compose
# mounts a tmpfs over it so the rest of the filesystem can stay read-only; the
# ownership set here only applies when the image is run without that mount.
RUN mkdir -p /run/play && chown play:play /run/play

USER play
WORKDIR /run/play

ENV PORT=8080 \
    FLOWCODE_FCC=/usr/local/bin/fcc \
    FLOWCODE_RUNNER=/usr/local/bin/fcplay \
    FLOWCODE_SAMPLES_DIR=/opt/flowcode/samples \
    PLAYGROUND_WORKDIR=/run/play \
    PLAYGROUND_WEB_ROOT=/srv/web

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/playground"]
