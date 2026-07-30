#!/usr/bin/env bash
#
# hole-balancer quick start — one command from nothing to a running balancer.
#
#   curl -fsSL https://raw.githubusercontent.com/chinmay28/hole-balancer/main/quickstart.sh | bash -s -- 192.168.1.10 192.168.1.11
#
# With no addresses it asks. Already have a checkout? ./quickstart.sh does the
# same thing without cloning.
#
# Options:
#   --install   install as a systemd service on port 53 instead of running in
#               the foreground on a high port
#
# Environment:
#   PORT, ADMIN_PORT      ports to listen on (default 5300, 8053)
#   HOLE_BALANCER_DIR     where to clone (default ./hole-balancer)
#   REPO_URL, BRANCH      where to clone from
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/chinmay28/hole-balancer}"
# Empty means "the remote default branch", which is what a local file:// clone
# or a fork with a differently-named default needs.
BRANCH="${BRANCH-main}"
CLONE_DIR="${HOLE_BALANCER_DIR:-hole-balancer}"

PORT="${PORT:-5300}"
ADMIN_PORT="${ADMIN_PORT:-8053}"
CONFIG="${CONFIG:-config.yaml}"
BINARY="./hole-balancer"
INSTALL=false

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
dim()  { printf '\033[2m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
die()  { printf '\033[31merror: %s\033[0m\n' "$*" >&2; exit 1; }

# Inlined rather than sed-ed out of the file: piped from curl there is no file.
usage() {
  cat <<'USAGE'
hole-balancer quick start — one command from nothing to a running balancer.

  curl -fsSL https://raw.githubusercontent.com/chinmay28/hole-balancer/main/quickstart.sh \
    | bash -s -- 192.168.1.10 192.168.1.11

Each argument is one Pi-hole. Give several addresses for the same machine as a
single quoted argument: "192.168.1.10 100.101.102.103". With no arguments it
asks. Inside a checkout, ./quickstart.sh does the same without cloning.

Options:
  --install    install as a systemd service on port 53, enabled at boot,
               instead of running in the foreground on a high port
  -h, --help   this text

Environment:
  PORT, ADMIN_PORT     ports to listen on (default 5300, 8053)
  HOLE_BALANCER_DIR    where to clone (default ./hole-balancer)
  REPO_URL, BRANCH     where to clone from; empty BRANCH takes the default
USAGE
}

servers=()
while [ $# -gt 0 ]; do
  case "$1" in
    --install) INSTALL=true; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) die "unknown option: $1" ;;
    *) servers+=("$1"); shift ;;
  esac
done

# ------------------------------------------------------- find the source ----

# Piped from curl there is no script file on disk, so BASH_SOURCE is empty or
# "bash". That is how we tell "run inside a checkout" from "bootstrap myself".
in_tree=false
src=""
case "${BASH_SOURCE[0]:-}" in
  ""|bash|sh|-*) ;;
  *) src="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd || true)" ;;
esac
if [ -n "$src" ] && [ -f "$src/go.mod" ] && [ -d "$src/cmd/hole-balancer" ]; then
  in_tree=true
  cd "$src"
fi

if ! $in_tree; then
  command -v git >/dev/null 2>&1 || die "git is required to fetch the source. Install git, or clone the repo by hand and run ./quickstart.sh"

  if [ -d "$CLONE_DIR/.git" ]; then
    bold "Updating $CLONE_DIR…"
    git -C "$CLONE_DIR" pull --ff-only --quiet ||
      warn "could not fast-forward $CLONE_DIR (local changes?) — building what is there"
  else
    bold "Fetching hole-balancer into $CLONE_DIR…"
    branch_arg=()
    [ -n "$BRANCH" ] && branch_arg=(--branch "$BRANCH")
    git clone --depth 1 "${branch_arg[@]}" --quiet "$REPO_URL" "$CLONE_DIR" ||
      die "clone failed: $REPO_URL ${BRANCH:+($BRANCH)}"
  fi
  cd "$CLONE_DIR"
fi

# ---------------------------------------------------------------- checks ----

command -v go >/dev/null 2>&1 || die "Go is required to build. Install it from https://go.dev/dl/"
dim "using $(go env GOVERSION 2>/dev/null || echo 'unknown go')"

if $INSTALL; then
  SUDO=""
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null 2>&1 || die "--install needs root; run as root or install sudo"
    SUDO=sudo
  fi
fi

# ----------------------------------------------------------- pi-hole list ----

if [ ${#servers[@]} -eq 0 ]; then
  # Piped from curl, stdin is the script itself, so a prompt has to read the
  # terminal directly. Test by opening it, not by stat-ing it: /dev/tty exists
  # as a device even with no controlling terminal, and only the open fails —
  # checking -c would print the whole prompt and then leak a shell error.
  if { exec 3</dev/tty; } 2>/dev/null; then
    bold "Which Pi-holes should share the load?"
    echo "One address per line — an IP or hostname, with an optional :port."
    echo "A Pi-hole reachable both on the LAN and over Tailscale goes on one line,"
    echo "both addresses separated by a space, so it counts once."
    echo "Press enter on an empty line when done."
    echo
    while true; do
      printf '  pi-hole %d> ' "${#servers[@]}"
      read -r line <&3 || break
      [ -z "$line" ] && break
      servers+=("$line")
    done
    exec 3<&-
  fi
fi

if [ ${#servers[@]} -eq 0 ]; then
  warn "No Pi-holes given — using 192.168.1.10 and 192.168.1.11 as placeholders."
  warn "Fix them in the web interface, or in $CONFIG, before relying on this."
  servers=("192.168.1.10" "192.168.1.11")
fi

# ----------------------------------------------------------------- build ----

bold "Building…"
CGO_ENABLED=0 go build -trimpath -o "$BINARY" ./cmd/hole-balancer
dim "  $(pwd)/${BINARY#./}"

# ---------------------------------------------------------------- config ----

# Installed as a service it owns port 53; run in the foreground it uses a high
# port so nothing needs privileges.
if $INSTALL; then
  listen_udp=":53"; listen_tcp=":53"
else
  listen_udp=":$PORT"; listen_tcp=":$PORT"
fi

if [ -f "$CONFIG" ]; then
  warn "$CONFIG already exists — leaving it alone."
else
  bold "Writing $CONFIG…"
  {
    echo "# Written by quickstart.sh. Full annotated reference: config.example.yaml"
    echo "listen:"
    echo "  udp: \"$listen_udp\""
    echo "  tcp: \"$listen_tcp\""
    echo
    echo "strategy: random"
    echo
    echo "admin:"
    echo "  listen: \"127.0.0.1:$ADMIN_PORT\""
    echo "  # Lets the web interface add, remove, and reconfigure Pi-holes."
    echo "  allow_control: true"
    echo
    echo "fallback:"
    echo "  # When no Pi-hole can answer, use public DNS so the network keeps"
    echo "  # working. These replies are NOT filtered. Set false to turn off."
    echo "  enabled: true"
    echo "  servers: [8.8.8.8, 8.8.4.4]"
    echo
    echo "upstreams:"
    i=1
    for entry in "${servers[@]}"; do
      echo "  - name: pihole-$i"
      echo "    endpoints:"
      # One line may hold several addresses for the same machine (LAN, Tailscale).
      for addr in $entry; do
        echo "      - $addr"
      done
      i=$((i + 1))
    done
  } > "$CONFIG"
fi

"$BINARY" -validate -config "$CONFIG" >/dev/null || die "generated config did not validate"

# ------------------------------------------------------------------- go ----

if $INSTALL; then
  echo
  $SUDO ./deploy/install.sh --config "$CONFIG"

  # Installed but unstartable is a real, distinguishable outcome — a container
  # or a WSL image without systemd as PID 1. Say so plainly instead of leaving
  # a raw "Failed to connect to bus" as the last thing on screen.
  if ! $SUDO systemctl enable --now hole-balancer; then
    echo
    warn "Files are installed, but systemd could not start the service here."
    echo "Once systemd is running:  sudo systemctl enable --now hole-balancer"
    exit 1
  fi

  echo
  bold "Running as a service on port 53."
  dim  "  Web    http://127.0.0.1:$ADMIN_PORT/"
  dim  "  Logs   journalctl -u hole-balancer -f"
  echo
  echo "Point your router's DHCP \"DNS server\" option at this host to move the"
  echo "whole network over as leases renew."
  exit 0
fi

echo
bold "Starting hole-balancer"
dim  "  DNS   127.0.0.1:$PORT  (udp + tcp)"
dim  "  Web   http://127.0.0.1:$ADMIN_PORT/"
echo
echo "Try it from another terminal:"
echo "  dig @127.0.0.1 -p $PORT example.com"
echo
echo "Happy with it? Re-run with --install to put it on port 53 as a service."
echo
dim "Ctrl-C to stop."
echo

exec "$BINARY" -config "$CONFIG"
