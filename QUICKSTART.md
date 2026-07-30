# Quick start

From nothing to a working DNS load balancer with a web interface, in about a
minute. You need [Go](https://go.dev/dl/) and at least one Pi-hole.

## One command

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/hole-balancer/main/quickstart.sh | bash -s -- 192.168.1.10 192.168.1.11
```

Put your own Pi-hole addresses on the end. That fetches the source, builds it,
writes a `config.yaml`, and starts up. Leave the addresses off and it asks.

Prefer to read it before running it — always a reasonable instinct with a piped
script:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/hole-balancer/main/quickstart.sh -o quickstart.sh
less quickstart.sh
bash quickstart.sh 192.168.1.10 192.168.1.11
```

Either way, two things are now listening:

| | |
|---|---|
| **DNS** | `127.0.0.1:5300` — udp and tcp |
| **Web** | <http://127.0.0.1:8053/> |

A high port is used so nothing needs root yet, and the source ends up in
`./hole-balancer`. Check it works:

```bash
dig @127.0.0.1 -p 5300 example.com
```

Then open <http://127.0.0.1:8053/> — the dashboard shows every Pi-hole, which
one is carrying the load, and lets you add or remove them without touching a
file.

### A Pi-hole on two networks

If a Pi-hole answers on both your LAN and Tailscale, give both addresses as one
argument, separated by a space:

```bash
... | bash -s -- "192.168.1.10 100.101.102.103" "192.168.1.11 100.101.102.104"
```

That records two machines with two routes each — not four machines. It matters:
the LAN address is used while it works, Tailscale takes over if it stops, and
each Pi-hole still gets its fair one-in-two share of the traffic.

### Different ports

```bash
curl -fsSL .../quickstart.sh | PORT=5301 ADMIN_PORT=8054 bash -s -- 192.168.1.10
```

### Already have a checkout?

`./quickstart.sh` does the same thing without cloning — it notices it is already
inside the source tree.

---

## The dashboard

<http://127.0.0.1:8053/> is the whole management interface. It refreshes every
five seconds and needs no build step, no monitoring stack, and no internet — it
is served by the balancer itself, which matters because you will be opening it
exactly when DNS is broken.

It works properly on a phone, which is usually what you have to hand when the
laptop has stopped resolving: everything reflows to one column, the buttons are
big enough to hit with a thumb, and tapping the chart gives you a reading.

**At the top** — total queries, how many Pi-holes are up, which one is busiest,
average response time, how much is being blocked, and how much has been answered
unfiltered by public DNS.

**Queries over time** — the last hour by minute or the last day by hour. Hover
for exact counts. When public DNS has been carrying traffic, a second dashed
line shows how much.

**Queries by Pi-hole** — who is doing the work, with a table view for exact
numbers.

**Pi-holes** — every server, its routes, which route is live, and its latency.
From here you can:

- **Add Pi-hole** — name it and paste its addresses, one per line.
- **Drain** — stop sending it queries without removing it. Perfect before a
  reboot or an update: no client ever waits on a timeout. Not saved to the
  config file, so an unrelated restart cannot leave a Pi-hole quietly out of
  rotation.
- **Remove** — take it out for good.
- **Strategy** — how the next query picks a Pi-hole.

**Public DNS fallback** — turn it on or off and edit the resolver list. While
it is carrying traffic the panel says so in red, because those answers are not
being filtered.

Every change applies immediately *and* is written back to `config.yaml`, so it
survives a restart. A copy of the previous file is kept as `config.yaml.bak`.

> The interface is editable because `quickstart.sh` sets `admin.allow_control:
> true`. There is no login, so it binds to loopback only. If you want it
> reachable from elsewhere on your network, put it behind something that
> authenticates — see the README.

---

## Making it permanent

Also one command — add `--install`:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/hole-balancer/main/quickstart.sh | sudo bash -s -- --install 192.168.1.10 192.168.1.11
```

That puts it on port 53 as a systemd service, enabled at boot. If you already
tried it in the foreground, re-run from the checkout to keep the config you
tuned:

```bash
cd hole-balancer && sudo ./quickstart.sh --install
```

Either way it ends up equivalent to:

```bash
sudo make install CONFIG=config.yaml
sudo systemctl enable --now hole-balancer
journalctl -u hole-balancer -f
```

That creates a `hole-balancer` system account, installs the binary and the unit,
copies your config to `/etc/hole-balancer/config.yaml` owned by that account,
and grants the binary `CAP_NET_BIND_SERVICE` so it binds port 53 without running
as root.

Upgrading later is `git pull && sudo make install` — an existing
`/etc/hole-balancer/config.yaml` is never overwritten, so nothing you changed by
hand or from the dashboard is lost. (`make uninstall` reverses it and leaves the
config behind.)

The dashboard stays editable under systemd: the unit grants write access to
`/etc/hole-balancer` specifically, since the rest of the filesystem is read-only
to the service.

**Then point your network at it.** Set your router's DHCP "DNS server" option to
the balancer's address, and the whole house moves over as leases renew.

> Don't point the balancer at itself. If it shares a host with a Pi-hole, keep
> them on different addresses or ports — a Pi-hole forwarding to a balancer that
> forwards back is a loop.

---

## Docker instead

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml            # set listen to ":5300", list your Pi-holes
docker compose up -d
```

The container runs unprivileged on 5300 and the host maps port 53 onto it.

---

## What next

- Trying a strategy: `random` spreads load evenly, `failover` keeps one Pi-hole
  primary, `least-latency` prefers whichever is fastest. Switch live from the
  dashboard and watch the "Queries by Pi-hole" chart change.
- Watching it work: `curl localhost:8053/summary` for a terminal-friendly view,
  `/metrics` for Prometheus.
- The full reference for every setting is `config.example.yaml`, and the
  reasoning behind each behaviour is in the [README](README.md).
