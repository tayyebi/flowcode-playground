#!/bin/sh
# Runs inside a plain golang:1.26-alpine container on every `docker compose
# up -d`. There is no Dockerfile and no prebuilt image: this script installs
# the toolchain, builds what the service role needs, and execs the result.
# Everything it touches — apk cache, FlowCode's git clone, Go's module/build
# caches, the compiled binaries — lives under $CACHE_DIR, which is inside
# the repo bind-mount (/src), so it's on the host and survives
# `docker compose down` / a fresh `git pull`. A repeat run only rebuilds
# what actually changed.
#
# Roles (first argument):
#   app    (default) the user-facing playground: builds the FlowCode
#          toolchain (fcc + fcplay) and the playground binary
#   admin  the administration area: builds only the admin binary — no
#          sandbox toolchain needed, so it starts fast
#
# The two roles use separate Go build caches so their first-boot builds
# never race on the shared host cache.
set -eu

ROLE="${1:-app}"
CACHE_DIR="/src/.buildcache"
FLOWCODE_SRC="$CACHE_DIR/flowcode-src"
BIN_DIR="$CACHE_DIR/bin"
FLOWCODE_REPO="${FLOWCODE_REPO:-https://github.com/tayyebi/flowcode.git}"
FLOWCODE_REF="${FLOWCODE_REF:-main}"

mkdir -p "$BIN_DIR" "$CACHE_DIR/go/pkg"

build_playground() {
    echo "==> apk: installing build toolchain (cache persisted on host)"
    apk add --update build-base git >/dev/null

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
    cat > /tmp/hello.fc <<'EOF'
workflow: HelloWorld

step greeting:
    emit
        value = "hello, world"
end

step saved:
    store set
        key = "greeting"
        value = greeting
end
EOF
    "$FLOWCODE_SRC/fcc" /tmp/hello.fc /tmp/hello.fcb
    "$BIN_DIR/fcplay" /tmp/hello.fcb 2>&1 | grep -q 'vm completed successfully'
    # hello.fc stores under "greeting": the server's KV log is parsed out of this
    # dump, so a drifted format must fail the build, not silently empty the log.
    "$BIN_DIR/fcplay" /tmp/hello.fcb 2>&1 | grep -q 'store set key = "greeting" value = "hello, world"'

    echo "==> server: building the playground binary"
    export GOPATH="$CACHE_DIR/go"
    export GOCACHE="$CACHE_DIR/go/build"
    (cd /src/server && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN_DIR/playground" .)

    export PORT="${PORT:-8033}"
    export FLOWCODE_FCC="$BIN_DIR/fcc"
    export FLOWCODE_RUNNER="$BIN_DIR/fcplay"
    export PLAYGROUND_WORKDIR="/run/play"
    mkdir -p "$PLAYGROUND_WORKDIR"

    echo "==> starting playground on :$PORT"
    exec "$BIN_DIR/playground"
}

build_admin() {
    echo "==> server: building the admin binary"
    export GOPATH="$CACHE_DIR/go"
    export GOCACHE="$CACHE_DIR/go/build-admin"
    (cd /src/server && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$BIN_DIR/admin" ./cmd/admin)

    export PORT="${PORT:-8034}"

    echo "==> starting the administration area on :$PORT"
    exec "$BIN_DIR/admin"
}

case "$ROLE" in
    app)    build_playground ;;
    admin)  build_admin ;;
    *)      echo "unknown role '$ROLE' (want: app | admin)" >&2; exit 2 ;;
esac
