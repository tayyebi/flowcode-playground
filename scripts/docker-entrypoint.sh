#!/bin/sh
# Runs inside a plain golang:1.25-alpine container on every `docker compose
# up -d`. There is no Dockerfile and no prebuilt image: this script installs
# the rest of the toolchain, builds FlowCode + the frontend + the server, and
# then execs the result. Everything it touches — apk cache, FlowCode's git
# clone, Go's module/build cache, npm's cache, the compiled binaries — lives
# under $CACHE_DIR, which is inside the repo bind-mount (/src), so it's on
# the host and survives `docker compose down` / a fresh `git pull`. A repeat
# run only rebuilds what actually changed; nothing gets redownloaded.
set -eu

CACHE_DIR="/src/.buildcache"
FLOWCODE_SRC="$CACHE_DIR/flowcode-src"
BIN_DIR="$CACHE_DIR/bin"
FLOWCODE_REPO="${FLOWCODE_REPO:-https://github.com/tayyebi/flowcode.git}"
FLOWCODE_REF="${FLOWCODE_REF:-main}"

mkdir -p "$BIN_DIR" "$CACHE_DIR/go/pkg" "$CACHE_DIR/go/build" "$CACHE_DIR/npm"

echo "==> apk: installing build toolchain (cache persisted on host)"
apk add --update build-base nodejs npm git >/dev/null

echo "==> flowcode: syncing $FLOWCODE_REPO @ $FLOWCODE_REF"
if [ -d "$FLOWCODE_SRC/.git" ]; then
    git -C "$FLOWCODE_SRC" fetch --depth 1 origin "$FLOWCODE_REF"
    git -C "$FLOWCODE_SRC" checkout -q FETCH_HEAD
else
    git clone --depth 1 --branch "$FLOWCODE_REF" "$FLOWCODE_REPO" "$FLOWCODE_SRC" \
        || (git clone "$FLOWCODE_REPO" "$FLOWCODE_SRC" && git -C "$FLOWCODE_SRC" checkout "$FLOWCODE_REF")
fi

echo "==> flowcode: building (make excludes the os plugin, same as before)"
make -C "$FLOWCODE_SRC"

echo "==> flowcode: building fcplay (the playground's trace driver)"
SOURCES="$(ls "$FLOWCODE_SRC"/src/*.c | grep -Ev '/(cli|compiler)\.c$')"
cc -std=c11 -Wall -Wextra -Werror -pedantic -O2 -I"$FLOWCODE_SRC/include" \
   -o "$BIN_DIR/fcplay" $SOURCES /src/runner/fcplay.c -ldl
cp "$FLOWCODE_SRC/fcc" "$FLOWCODE_SRC/flowcode" "$BIN_DIR/"

echo "==> flowcode: smoke-checking the trace driver"
"$FLOWCODE_SRC/fcc" "$FLOWCODE_SRC/samples/hello-world/hello.fc" /tmp/hello.fcb
"$BIN_DIR/fcplay" /tmp/hello.fcb 2>&1 | grep -q 'vm completed successfully'

echo "==> web: building frontend"
export npm_config_cache="$CACHE_DIR/npm"
(cd /src/web && npm ci --prefer-offline && npm run build)

echo "==> server: building"
export GOPATH="$CACHE_DIR/go"
export GOCACHE="$CACHE_DIR/go/build"
(cd /src/server && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN_DIR/playground" .)

export PORT="${PORT:-8033}"
export FLOWCODE_FCC="$BIN_DIR/fcc"
export FLOWCODE_RUNNER="$BIN_DIR/fcplay"
export FLOWCODE_SAMPLES_DIR="$FLOWCODE_SRC/samples"
export PLAYGROUND_WORKDIR="/run/play"
export PLAYGROUND_WEB_ROOT="/src/web/dist"
export PLAYGROUND_DB_PATH="${PLAYGROUND_DB_PATH:-/data/playground.db}"
mkdir -p "$PLAYGROUND_WORKDIR"

echo "==> starting playground on :$PORT"
exec "$BIN_DIR/playground"
