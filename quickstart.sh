#!/usr/bin/env bash
#
# hole-balancer quick start.
#
# Builds the balancer, writes a config for the Pi-holes you name, and starts it
# on a high port so nothing needs root. Run with no arguments for a guided
# setup, or pass addresses directly:
#
#   ./quickstart.sh 192.168.1.10 192.168.1.11
#
set -euo pipefail

PORT="${PORT:-5300}"
ADMIN_PORT="${ADMIN_PORT:-8053}"
CONFIG="${CONFIG:-config.yaml}"
BINARY="./hole-balancer"

bold()  { printf '\033[1m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }
warn()  { printf '\033[33m%s\033[0m\n' "$*"; }
die()   { printf '\033[31merror: %s\033[0m\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")"

# ---------------------------------------------------------------- checks ----

command -v go >/dev/null 2>&1 || die "Go is required to build. Install it from https://go.dev/dl/"

go_version=$(go env GOVERSION 2>/dev/null || echo "unknown")
dim "using $go_version"

# ---------------------------------------------------------- pi-hole list ----

servers=("$@")

if [ ${#servers[@]} -eq 0 ]; then
  bold "Which Pi-holes should share the load?"
  echo "Enter one address per line — an IP or hostname, with an optional :port."
  echo "A Pi-hole reachable both on the LAN and over Tailscale should be entered"
  echo "as one line with both addresses separated by a space, so it counts once."
  echo "Press enter on an empty line when done."
  echo
  while true; do
    read -r -p "  pi-hole ${#servers[@]}> " line || break
    [ -z "$line" ] && break
    servers+=("$line")
  done
fi

if [ ${#servers[@]} -eq 0 ]; then
  warn "No Pi-holes given, using 192.168.1.10 and 192.168.1.11 as placeholders."
  warn "Edit $CONFIG (or use the web interface) before relying on this."
  servers=("192.168.1.10" "192.168.1.11")
fi

# ------------------------------------------------------------- build ----

bold "Building…"
CGO_ENABLED=0 go build -trimpath -o "$BINARY" ./cmd/hole-balancer
dim "built $BINARY"

# ------------------------------------------------------------ config ----

if [ -f "$CONFIG" ]; then
  warn "$CONFIG already exists — leaving it alone."
else
  bold "Writing $CONFIG…"
  {
    echo "# Written by quickstart.sh. Full annotated reference: config.example.yaml"
    echo "listen:"
    echo "  udp: \":$PORT\""
    echo "  tcp: \":$PORT\""
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
      # A line may hold several addresses for the same machine (LAN, Tailscale).
      for addr in $entry; do
        echo "      - $addr"
      done
      i=$((i + 1))
    done
  } > "$CONFIG"
fi

"$BINARY" -validate -config "$CONFIG" >/dev/null || die "generated config did not validate"

# ------------------------------------------------------------- run ----

echo
bold "Starting hole-balancer"
dim  "  DNS   127.0.0.1:$PORT  (udp + tcp)"
dim  "  Web   http://127.0.0.1:$ADMIN_PORT/"
echo
echo "Try it from another terminal:"
echo "  dig @127.0.0.1 -p $PORT example.com"
echo
echo "When you are happy with it, open $CONFIG, set listen to \":53\", and see"
echo "the README for running it as a service."
echo
dim "Ctrl-C to stop."
echo

exec "$BINARY" -config "$CONFIG"
