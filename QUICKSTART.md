# Quick start

From nothing to a working DNS load balancer with a web interface, in about a
minute. You need [Go](https://go.dev/dl/) and at least one Pi-hole.

## One command

```bash
git clone https://github.com/chinmay28/hole-balancer
cd hole-balancer
./quickstart.sh
```

It asks which Pi-holes to balance across, builds the binary, writes a
`config.yaml`, and starts up. Or skip the questions:

```bash
./quickstart.sh 192.168.1.10 192.168.1.11
```

That's it. Two things are now listening:

| | |
|---|---|
| **DNS** | `127.0.0.1:5300` — udp and tcp |
| **Web** | <http://127.0.0.1:8053/> |

A high port is used so nothing needs root yet. Check it works:

```bash
dig @127.0.0.1 -p 5300 example.com
```

Then open <http://127.0.0.1:8053/> — the dashboard shows every Pi-hole, which
one is carrying the load, and lets you add or remove them without touching a
file.

### A Pi-hole on two networks

If a Pi-hole answers on both your LAN and Tailscale, give both addresses on one
line, separated by a space:

```bash
./quickstart.sh "192.168.1.10 100.101.102.103" "192.168.1.11 100.101.102.104"
```

That records two machines with two routes each — not four machines. It matters:
the LAN address is used while it works, Tailscale takes over if it stops, and
each Pi-hole still gets its fair one-in-two share of the traffic.

### Different ports

```bash
PORT=5301 ADMIN_PORT=8054 ./quickstart.sh 192.168.1.10
```

---

## The dashboard

<http://127.0.0.1:8053/> is the whole management interface. It refreshes every
five seconds and needs no build step, no monitoring stack, and no internet — it
is served by the balancer itself, which matters because you will be opening it
exactly when DNS is broken.

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

Once you are happy with the setup:

**1. Move it to port 53.** Edit `config.yaml`:

```yaml
listen:
  udp: ":53"
  tcp: ":53"
```

**2. Install it as a service:**

```bash
sudo make install
sudo cp config.yaml /etc/hole-balancer/config.yaml
sudo systemctl enable --now hole-balancer
journalctl -u hole-balancer -f
```

The bundled unit binds port 53 with `CAP_NET_BIND_SERVICE` rather than running
as root.

**3. Point your network at it.** Set your router's DHCP "DNS server" option to
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
