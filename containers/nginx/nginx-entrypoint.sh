#!/bin/sh
# Runs inside the nginx container. Generates a self-signed TLS certificate
# on first boot — only if one doesn't already exist — into /certs, which is
# the host's git-ignored ./certs directory. Certificates therefore persist
# across restarts and rebuilds, and replacing them with real ones is a
# drop-in: overwrite the two files and restart the service; nginx just
# loads whatever is there.
set -eu

CERT_DIR="/certs"
FULLCHAIN="$CERT_DIR/fullchain.pem"
PRIVKEY="$CERT_DIR/privkey.pem"

if [ ! -f "$FULLCHAIN" ] || [ ! -f "$PRIVKEY" ]; then
    echo "==> certs: no certificate found in $CERT_DIR, generating a self-signed pair"
    command -v openssl >/dev/null 2>&1 || apk add --no-cache openssl >/dev/null

    SAN="DNS:${CERT_CN:-localhost}"
    [ -n "${CERT_DNS:-}" ] && SAN="$SAN,DNS:$CERT_DNS"
    [ -n "${CERT_IP:-}" ] && SAN="$SAN,IP:$CERT_IP"

    tmp="$(mktemp -d)"
    openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
        -keyout "$tmp/privkey.pem" -out "$tmp/fullchain.pem" \
        -subj "/CN=${CERT_CN:-localhost}" \
        -addext "subjectAltName=$SAN" >/dev/null 2>&1
    mv "$tmp/privkey.pem" "$PRIVKEY"
    mv "$tmp/fullchain.pem" "$FULLCHAIN"
    rmdir "$tmp"
    echo "==> certs: generated ($SAN)"
else
    echo "==> certs: using existing certificate from $CERT_DIR"
fi

echo "==> nginx: starting"
exec nginx -g 'daemon off;'
