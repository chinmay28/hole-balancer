#!/usr/bin/env bash
#
# Install hole-balancer as a systemd service.
#
# Called by `make install`. Run directly if you want to pass options:
#
#   sudo ./deploy/install.sh                       # seed an example config
#   sudo ./deploy/install.sh --config config.yaml  # seed the config you already have
#
# Safe to re-run: an existing configuration is never overwritten, so upgrading
# is just `git pull && sudo make install`.
#
# Packagers: set DESTDIR to stage into a build root. That also skips everything
# that only makes sense on a live system — creating the account, setting the
# capability, and reloading systemd.
set -euo pipefail

BINARY_NAME=hole-balancer
SERVICE_USER=hole-balancer
SERVICE_GROUP=hole-balancer

DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr/local}"
BINDIR="${BINDIR:-$PREFIX/bin}"
SYSCONFDIR="${SYSCONFDIR:-/etc}"
UNITDIR="${UNITDIR:-/etc/systemd/system}"

CONFDIR="$SYSCONFDIR/hole-balancer"
CONFFILE="$CONFDIR/config.yaml"
SEED=""

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
dim()  { printf '\033[2m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
die()  { printf '\033[31merror: %s\033[0m\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --config) SEED="${2:-}"; shift 2 ;;
    --config=*) SEED="${1#*=}"; shift ;;
    -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

cd "$(dirname "$0")/.."

[ -x "./$BINARY_NAME" ] || die "./$BINARY_NAME not found — run 'make build' first"

# A staged install writes into a build root, where there is no account to
# create, no running systemd, and no point setting a capability.
staged=false
[ -n "$DESTDIR" ] && staged=true

if ! $staged && [ "$(id -u)" -ne 0 ]; then
  die "must run as root (try: sudo make install)"
fi

# ------------------------------------------------------------- account ----

if $staged; then
  dim "staged install into $DESTDIR — skipping account creation"
elif getent passwd "$SERVICE_USER" >/dev/null 2>&1; then
  dim "service account $SERVICE_USER already exists"
else
  bold "Creating the $SERVICE_USER system account…"
  if command -v useradd >/dev/null 2>&1; then
    getent group "$SERVICE_GROUP" >/dev/null 2>&1 || groupadd --system "$SERVICE_GROUP"
    useradd --system --gid "$SERVICE_GROUP" \
            --home-dir /nonexistent --no-create-home \
            --shell /usr/sbin/nologin \
            --comment "hole-balancer DNS load balancer" \
            "$SERVICE_USER"
  elif command -v adduser >/dev/null 2>&1; then
    # busybox / Alpine
    getent group "$SERVICE_GROUP" >/dev/null 2>&1 || addgroup -S "$SERVICE_GROUP"
    adduser -S -D -H -G "$SERVICE_GROUP" -s /sbin/nologin "$SERVICE_USER"
  else
    die "no useradd or adduser found; create the '$SERVICE_USER' system user by hand and re-run"
  fi
fi

# ------------------------------------------------------------- files ----

bold "Installing…"

install -Dm755 "./$BINARY_NAME" "$DESTDIR$BINDIR/$BINARY_NAME"
dim "  $BINDIR/$BINARY_NAME"

install -Dm644 deploy/$BINARY_NAME.service "$DESTDIR$UNITDIR/$BINARY_NAME.service"
dim "  $UNITDIR/$BINARY_NAME.service"

install -d -m750 "$DESTDIR$CONFDIR"

# Never clobber a live configuration. The management interface writes to this
# file, so overwriting on upgrade would throw away every change made from the
# dashboard as well as anything edited by hand.
if [ -e "$DESTDIR$CONFFILE" ]; then
  dim "  $CONFFILE already exists — left untouched"
else
  src="${SEED:-config.example.yaml}"
  [ -f "$src" ] || die "config to install not found: $src"
  install -m640 "$src" "$DESTDIR$CONFFILE"
  dim "  $CONFFILE  (from $src)"
fi

# The service must be able to read the config, and to rewrite it when
# admin.allow_control is on. Save() writes a temporary file in this directory
# and renames it, so the directory needs to be writable too, not just the file.
if ! $staged; then
  chown -R "$SERVICE_USER:$SERVICE_GROUP" "$CONFDIR"
  setcap cap_net_bind_service=+ep "$BINDIR/$BINARY_NAME" ||
    warn "setcap failed — the service will not be able to bind port 53. Either install libcap2-bin, or use a high port in $CONFFILE."
fi

# --------------------------------------------------------------- done ----

# A container, a chroot, or WSL without systemd has nothing to reload. That is
# not a reason to fail an install that has otherwise completed.
if ! $staged && command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload 2>/dev/null ||
    warn "could not reload systemd (not running as init here?) — run 'systemctl daemon-reload' once it is available"
fi

echo
if [ -n "$SEED" ]; then
  bold "Installed. Start it with:"
else
  bold "Installed. Edit $CONFFILE, then start it with:"
fi
echo "  sudo systemctl enable --now $BINARY_NAME"
echo "  journalctl -u $BINARY_NAME -f"
