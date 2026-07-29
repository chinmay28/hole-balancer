# hole-balancer

A DNS load balancer for a pool of Pi-hole servers.

Point your router or clients at `hole-balancer` instead of at one Pi-hole. It
spreads queries across every Pi-hole that is answering, and routes around the
ones that are not — so rebooting a Pi-hole, or losing one entirely, stops being
something the whole house notices.

It is a single static binary with no runtime dependencies, it ships with a web
interface for managing the pool, and it forwards DNS messages **byte for
byte**: EDNS0, DNSSEC, and cookies pass through exactly as the client and the
Pi-hole intended.

**[→ Quick start](QUICKSTART.md)** — running in about a minute.

---

## What it does

- **Spreads load.** Every query goes to a randomly chosen healthy Pi-hole, so
  no single one carries the whole network. Weighted, round-robin, latency-based,
  and strict-failover selection are available too.
- **Survives outages.** A Pi-hole that stops answering is taken out of rotation
  in milliseconds, and put back automatically once it recovers.
- **Handles multi-homed servers.** A Pi-hole reachable over both LAN and
  Tailscale is one upstream with two paths. The LAN path is used while it works,
  Tailscale takes over when it does not, and the Pi-hole still counts once in
  the load spread.
- **Retries in-flight.** If the chosen Pi-hole times out, the query is re-asked
  elsewhere before the client ever sees a failure.
- **Never black-holes DNS.** If the balancer believes *every* upstream is down,
  it tries them anyway rather than failing the network — a stale health verdict
  is a worse outcome than a wasted round trip.
- **Falls back to public DNS.** When no Pi-hole can answer at all, queries go to
  Google's resolvers (or whichever you configure) so the network keeps working
  through a total outage. Usage is reported as one daily summary, never as a
  line per query.
- **Comes with a dashboard.** Add and remove Pi-holes, drain one for
  maintenance, edit the fallback list, switch strategy, and see who is handling
  what — no config file, no restart. Served by the balancer itself, so it loads
  even when DNS is down.
- **Reports what it is doing.** A web dashboard, JSON status, Prometheus
  metrics, and a plain text summary you can `curl`.

## What it does not do

- **It does not cache.** Your Pi-holes already do, and a second cache in front
  would only make TTLs harder to reason about.
- **It does not filter.** Blocking is Pi-hole's job; this only decides *which*
  Pi-hole answers.
- **It does not merge query logs.** Each Pi-hole logs the share it served. With
  the balancer in front, they will all see the balancer's IP as the client
  unless you deploy it with host networking.
- **It does not speak DoH or DoT.** Plain DNS over UDP and TCP, on both sides.

---

## Quick start

```bash
git clone https://github.com/chinmay28/hole-balancer
cd hole-balancer
./quickstart.sh 192.168.1.10 192.168.1.11
```

That builds it, writes a config, and starts on a high port with the dashboard at
<http://127.0.0.1:8053/>. See [QUICKSTART.md](QUICKSTART.md) for the guided
version and how to make it permanent.

Or do it by hand:

```bash
make build
cp config.example.yaml config.yaml
$EDITOR config.yaml          # list your Pi-holes under `upstreams`

./hole-balancer -validate -config config.yaml
sudo ./hole-balancer -config config.yaml
```

A minimal `config.yaml` is just this:

```yaml
upstreams:
  - name: pihole-1
    endpoints: [192.168.1.10]
  - name: pihole-2
    endpoints: [192.168.1.11]
```

Everything else has a working default. Check it is answering:

```bash
dig @127.0.0.1 example.com
open http://localhost:8053/      # the dashboard
curl -s localhost:8053/summary   # or the same thing as text
```

Then set your router's DHCP "DNS server" option to the balancer's address, or
point individual clients at it.

> **Do not point the balancer at itself.** If it runs on the same host as a
> Pi-hole, give the two different addresses or ports — a Pi-hole configured to
> forward to the balancer that forwards back to that Pi-hole is a loop.

---

## Configuration

`config.example.yaml` documents every option inline. The parts worth
understanding:

### Upstreams and endpoints

An **upstream** is one physical Pi-hole. An **endpoint** is one way to reach it.

```yaml
upstreams:
  - name: pihole-living-room
    weight: 1
    endpoints:
      - 192.168.1.10           # tried first
      - 100.101.102.103        # Tailscale, used only if the LAN path fails
```

Listing both addresses under one upstream — rather than as two upstreams — is
what makes this correct: the same machine would otherwise take two slots in the
random draw and receive twice its share of traffic. Configuring the same
address under two upstreams is rejected at startup for the same reason.

Endpoints are tried in order, so put the fast path first.

### Choosing a strategy

| `strategy` | Behaviour | Use when |
|---|---|---|
| `random` *(default)* | Weighted random draw over healthy upstreams | You want load spread and no shared state |
| `round-robin` | Strict rotation | You want an exactly even split |
| `failover` | Always the first healthy upstream listed | One Pi-hole is primary, the rest are spares |
| `least-latency` | Lowest measured round-trip time | Mixed LAN and remote upstreams |

Use `weight` to give a beefier Pi-hole a larger share:

```yaml
  - name: pihole-nuc
    weight: 3          # gets ~3x the traffic of a weight-1 upstream
```

### Health checking

Two signals decide whether an endpoint is in rotation:

- **Active probes** every `health.interval` against *every* endpoint, including
  ones already marked down — that is the only way a recovered Pi-hole comes back.
- **Passive observation** of real client queries, which notices a dead Pi-hole
  in milliseconds instead of waiting for the next interval.

An endpoint needs `health.fall` consecutive failures to drop out and
`health.rise` consecutive successes to return, which keeps a lossy Wi-Fi link
from flapping it in and out.

A probe counts as **healthy if it gets any well-formed DNS reply**, including
`NXDOMAIN`. This is deliberate: the probe name may be on one Pi-hole's
blocklist and not another's, and a filtered name proves the server is working.
Only silence, `SERVFAIL`, `REFUSED`, or `NOTIMP` count as an outage.

Set `health.probe.require_answer: true` if you want the stricter check — it
additionally proves the Pi-hole's own upstream resolver is working, at the cost
of a false outage if the probe name is ever blocked.

### When every Pi-hole is down

If no Pi-hole can answer — all of them rebooting, a switch down, a power cut —
the balancer sends the query to a public resolver rather than failing it:

```yaml
fallback:
  enabled: true            # on by default
  servers:
    - 8.8.8.8
    - 8.8.4.4
  timeout: 2s
  summary_interval: 24h
```

**These answers are not filtered.** A public resolver knows nothing about your
blocklists, so while fallback is carrying traffic, ads are not blocked. That is
the trade, and it is deliberate: a house where nothing resolves is a worse
failure than a house that briefly sees ads. Set `enabled: false` if you would
rather DNS fail than go unfiltered.

Three things keep this honest:

- It only ever runs after every Pi-hole attempt has failed. While any Pi-hole
  answers, no query reaches a public resolver — verified by a test.
- Blocked domains never trigger it. Pi-hole reports a block as `NXDOMAIN` or an
  answer, both of which are success, so the query never falls through.
- Once the pool is known to be fully down, the balancer spends a single attempt
  confirming that before going to public DNS, instead of burning the whole
  retry budget on every query. One attempt still catches a Pi-hole that
  recovered before the health checker noticed.

Resolvers are tried in rotation, so a long outage spreads across them.

### Knowing when it happened

Fallback is never logged per query — an hour-long outage would be tens of
thousands of identical lines. Instead usage accumulates and is written once per
`summary_interval` (daily by default):

```
level=WARN msg="public DNS fallback was used: these queries were NOT filtered by Pi-hole"
  window=24h0m0s since=2026-07-28T00:00:00Z queries=18432 failed=3
  resolvers="8.8.8.8:53=9310 8.8.4.4:53=9119"
  outages=2 outage_total=41m18s outage_longest=38m2s still_down=false
```

- A day with no fallback logs **nothing at all**, so a line appearing is itself
  the signal.
- An outage still in progress is reported as `still_down=true` with the time so
  far, and continues into the next window without being double-counted.
- A pending summary is flushed on shutdown, so a restart never loses it.

Per-Pi-hole outages are still logged as they happen (`endpoint state changed`),
so the daily summary is about *unfiltered answers*, not about losing one
server. Live state is on the admin interface at any time:

```bash
curl -s localhost:8053/status | jq .fallback
{
  "enabled": true,
  "active": false,
  "servers": ["8.8.8.8:53", "8.8.4.4:53"],
  "window_start": "2026-07-29T00:00:00Z",
  "queries_this_window": 0,
  "outages_this_window": 0
}
```

### Retries and your blocklists

When an upstream returns one of `query.retry_rcodes` (`SERVFAIL`, `REFUSED` by
default), the query is re-asked against a different Pi-hole. **This cannot
weaken your blocking.** A domain that Pi-hole blocks comes back as `NXDOMAIN`
or as an answer pointing at `0.0.0.0` — never as `SERVFAIL` — so a blocked
result is returned to the client untouched.

Do not add `NXDOMAIN` to `retry_rcodes`. That would make the balancer shop
around until some upstream failed to block a domain.

If your Pi-holes have *different* blocklists, note that a given domain's fate
then depends on which one answers. Keep the lists in sync (Gravity Sync, or a
shared config) if you want deterministic blocking.

---

## Deployment

### systemd

```bash
sudo make install
sudo $EDITOR /etc/hole-balancer/config.yaml
sudo systemctl enable --now hole-balancer
journalctl -u hole-balancer -f
```

The bundled unit runs the balancer as a transient unprivileged user with
`CAP_NET_BIND_SERVICE`, so it binds port 53 without ever being root, and with a
tight sandbox (read-only filesystem, no new privileges, restricted syscalls).

If some upstreams are only reachable over Tailscale, uncomment the
`After=tailscaled.service` line so the startup health sweep can see them.

### Docker

```bash
cp config.example.yaml config.yaml
$EDITOR config.yaml          # set listen.udp and listen.tcp to ":5300"
docker compose up -d
```

The image is distroless and runs unprivileged, so it listens on 5300 and the
host maps port 53 onto it. Switch to `network_mode: host` if you would rather
your Pi-holes see real client IPs in their query logs.

### Binding port 53

Port 53 is privileged. Pick one:

```bash
# grant just the one capability (preferred)
sudo setcap cap_net_bind_service=+ep /usr/local/bin/hole-balancer
```

```yaml
# or use a high port and redirect to it
listen:
  udp: ":5300"
  tcp: ":5300"
```

The balancer detects this failure and says so, rather than exiting with a bare
"permission denied".

---

## Operating it

### The dashboard

<http://localhost:8053/> is the management interface: pool state, live
statistics, and the controls to change any of it. It is a single self-contained
page served from the binary — no CDN, no build step, nothing fetched over the
network. That is deliberate: you open this page when DNS is broken.

It shows total queries and the recent rate, how many Pi-holes are up, **which
one has handled the most**, average response time, the share of queries blocked,
how many were answered unfiltered by public DNS, a queries-over-time chart
(hour by minute, or day by hour), a per-Pi-hole breakdown with a table view, and
the response-code and query-type mix.

With `admin.allow_control: true` it is also editable — add a Pi-hole, remove
one, drain one before a reboot, edit the fallback resolvers, or switch strategy.
Changes apply immediately **and** are written back to your config file, with the
previous version kept as `<config>.bak`.

Two things to know about that write-back:

- **Comments are not preserved.** Once you edit through the interface the file
  is machine-managed; keep notes elsewhere. The annotated reference always lives
  in `config.example.yaml`.
- **Draining is not saved.** It is a "while I work on this box" state, and a
  Pi-hole still drained after an unrelated reboot is a trap.

**There is no authentication.** Keep `admin.listen` on loopback (the default)
unless you put something in front of it that authenticates. With
`allow_control: false` — also the default — the interface is read-only and every
mutating endpoint returns 403.

### From the terminal

```bash
curl -s localhost:8053/summary   # human-readable summary
curl -s localhost:8053/status    # the same as JSON
curl -s localhost:8053/healthz   # 200 while any Pi-hole answers, 503 otherwise
curl -s localhost:8053/metrics   # Prometheus
```

The management API behind the dashboard is plain JSON, so anything can drive it:

| Endpoint | Does |
|---|---|
| `GET /api/overview` | pool state, health, fallback, what you are allowed to change |
| `GET /api/stats` | every statistic the dashboard shows, including history |
| `GET /api/config` | the effective configuration |
| `POST /api/upstreams` | add a Pi-hole — `{"name":…,"weight":1,"endpoints":[…]}` |
| `DELETE /api/upstreams/{name}` | remove one |
| `POST /api/upstreams/{name}/drain` | `{"drained":true}` |
| `PUT /api/strategy` | `{"strategy":"least-latency"}` |
| `PUT /api/fallback` | `{"enabled":true,"servers":[…]}` |
| `POST /api/stats/reset` | clear the counters |

```bash
curl -sXPOST localhost:8053/api/upstreams \
  -d '{"name":"pihole-attic","endpoints":["192.168.1.12","100.101.102.105"]}'
```

```
$ curl -s localhost:8053/
hole-balancer v1.0.0

strategy: random
upstreams: 2 healthy of 3

pihole-living-room   UP               weight=1
  * 192.168.1.10:53           up        1.2ms  queries=8431 failures=0
    100.101.102.103:53        up       24.7ms  queries=12   failures=0
pihole-office        UP               weight=1
  * 192.168.1.11:53           up        1.4ms  queries=8388 failures=2
pihole-spare         DOWN             weight=1
    192.168.1.12:53           down      0.0ms  queries=0 failures=0  reason="read: i/o timeout"
```

The `*` marks the path currently carrying traffic for that upstream.

### Draining a Pi-hole for maintenance

Use the **Drain** button on the dashboard, or:

```bash
curl -sXPOST localhost:8053/api/upstreams/pihole-office/drain -d '{"drained":true}'
# ... update, reboot, whatever ...
curl -sXPOST localhost:8053/api/upstreams/pihole-office/drain -d '{"drained":false}'
```

Draining takes effect immediately, so no client ever waits on a timeout.

(The older `POST /drain?upstream=NAME` form still works.)

### Metrics worth alerting on

| Metric | Meaning |
|---|---|
| `holebalancer_upstream_up` | 0 when a Pi-hole has no working path |
| `holebalancer_fallback_active` | 1 while answers are coming from public DNS, unfiltered |
| `holebalancer_fallback_responses_total` | Queries served by a public resolver |
| `holebalancer_servfail_total` | Queries no upstream could answer — should stay flat |
| `holebalancer_retries_total` | Rising means an upstream is flaky |
| `holebalancer_query_duration_seconds` | End-to-end latency, including retries |
| `holebalancer_endpoint_latency_seconds` | Per-path round-trip time |
| `holebalancer_responses_total` | Answers by upstream and response code |

A reasonable first alert is `holebalancer_upstream_up == 0` for any upstream,
plus `rate(holebalancer_servfail_total[5m]) > 0`. If you care about blocking
staying on, alert on `holebalancer_fallback_active == 1` as well — that gauge
is high exactly when nothing is being filtered.

---

## How a query is handled

1. A datagram arrives. Anything shorter than a DNS header, or with the response
   bit already set, is dropped — the balancer will not act as a reflector.
2. `Plan()` builds an ordered list of endpoints to try:
   - the preferred path of each healthy upstream, ordered by the strategy, so
     the first entry is the selected Pi-hole;
   - the remaining healthy paths, shuffled;
   - everything believed to be down, shuffled — the fail-open tier.
3. The query is forwarded verbatim to the first entry, on the same transport the
   client used, with its own socket and its own deadline.
4. A well-formed reply is returned to the client unmodified. A timeout, a
   network error, or a retryable response code moves on to the next entry, up to
   `query.max_attempts`.
5. Every outcome updates that endpoint's health counters and latency average.
6. If every attempt fails and fallback is enabled, the query goes to a public
   resolver, and the fact is counted for the next summary.
7. If that fails too, the client gets `SERVFAIL` rather than silence.

A retry moves to a *different Pi-hole* before it falls back to a second path to
the same one: if a host is down, another route to it will not help.

---

## Development

```bash
make test        # unit and integration tests
make race        # the same under the race detector
make cover       # coverage summary
make all         # fmt, vet, test, build
make dist        # cross-compile for linux/amd64, arm64, arm, and darwin/arm64
```

The tests run real DNS servers on loopback (`internal/testdns`) and drive the
balancer through its actual sockets, including the failure cases: dropped
queries, `SERVFAIL` storms, slow upstreams, and every upstream dying at once.
The fallback tests point at fake resolvers and the summary tests drive an
injected clock, so nothing in the suite touches the real 8.8.8.8 or waits a day
to check a daily report.

Layout:

| Package | Responsibility |
|---|---|
| `internal/dnsmsg` | The slice of DNS wire format needed to read headers and questions, build probes, and synthesise errors |
| `internal/dnsclient` | One exchange with one upstream, over UDP or TCP |
| `internal/config` | Loading and validating YAML |
| `internal/pool` | Upstreams, endpoints, health state, and selection |
| `internal/health` | Active probing |
| `internal/fallback` | Public-DNS last resort and its daily usage summary |
| `internal/stats` | Query statistics and the short history the dashboard charts |
| `internal/control` | Applies dashboard changes to the running server and to disk |
| `internal/proxy` | Listeners and the forwarding path |
| `internal/admin` | Dashboard, management API, status, and metrics |
| `internal/metrics` | Counters, histograms, Prometheus rendering |

The only dependency is `gopkg.in/yaml.v3`.

## Licence

MIT. See [LICENSE](LICENSE).
