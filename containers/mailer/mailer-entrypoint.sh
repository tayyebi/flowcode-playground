#!/bin/sh
# Runs inside the mailer container (stock alpine, like nginx — no Dockerfile,
# no prebuilt image). Installs Postfix at boot, renders main.cf from the
# mounted template, disables chroot (the container is the jail), and runs a
# send-only relay in the foreground. Postfix logs to the container's stdout;
# the queue lives on the mailer-spool volume.
set -eu

MAIL_NAME="${MAIL_NAME:?MAIL_NAME is required — the sending hostname, must match the host's PTR record and the domain's SPF}"
MYNETWORKS="${MYNETWORKS:-127.0.0.0/8, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16}"

echo "==> mailer: installing postfix"
apk add --no-cache postfix

echo "==> mailer: rendering /etc/postfix/main.cf (myhostname=$MAIL_NAME)"
sed -e "s|@MAIL_NAME@|$MAIL_NAME|g" \
    -e "s|@MYNETWORKS@|$MYNETWORKS|g" \
    /mailer-main.cf > /etc/postfix/main.cf

# No chroot: the package's chroot tree lives under /var/spool/postfix, which
# a fresh mailer-spool volume replaces with an empty directory — and the
# container is the jail anyway.
echo "==> mailer: disabling chroot in master.cf"
for svc in $(postconf -M | awk '{print $1"/"$2}'); do
    postconf -F "$svc/chroot=n" >/dev/null
done

# Make sure the queue layout exists and is owned correctly before master
# starts (best-effort — the healthcheck is the arbiter). The spool lives on
# a named volume, not a host bind mount: Postfix enforces postfix/postdrop
# ownership of the spool, which a bind mount cannot carry on macOS.
"$(postconf -h daemon_directory)/post-install" create missing 2>/dev/null || true
postfix set-permissions 2>/dev/null || true
newaliases 2>/dev/null || true

echo "==> mailer: starting send-only relay (direct-to-MX, no credentials)"
exec postfix start-fg
