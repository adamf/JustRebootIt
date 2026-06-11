# JustRebootIt

A self-contained, Dockerized recorder for **intermittent home-internet latency
spikes** — the kind where everything feels fine until a video call freezes for
three seconds, and by the time you run a speed test it's gone.

It continuously measures latency, jitter and packet loss to a spread of diverse
targets (your gateway, your ISP, and well-run public anchors), traces the
network path to find *where* the latency lives, and — if you have a **UniFi
Dream Machine Pro** — correlates all of that against WAN throughput and gateway
load. Everything lands on **one Grafana dashboard** designed to be screenshotted
or link-shared with a network engineer at your ISP (e.g. Comcast).

The probes are written in Go and run fully in parallel, so dozens of targets are
measured on a tight cycle without the prober itself becoming the bottleneck.

```
                 ┌─────────────┐     ICMP ping + traceroute
                 │   prober    │────────────────────────────►  many targets
                 │   (Go)      │                                (gateway, ISP, anchors)
                 └──────┬──────┘
                        │ /metrics
   UniFi Dream   ┌──────┴──────┐
   Machine Pro ─►│ udm-exporter│  WAN throughput, gateway CPU/mem,
       (API)     │   (Go)      │  speedtest, client counts
                 └──────┬──────┘
                        │ /metrics
                 ┌──────┴──────┐        ┌─────────────┐
                 │ prometheus  │───────►│   grafana   │  http://localhost:3000
                 │  (90d TSDB) │        │ (1 dashboard)│  → lands on the dashboard
                 └─────────────┘        └─────────────┘
```

## Quick start

```sh
git clone <this repo> && cd JustRebootIt
cp .env.example .env
$EDITOR .env                 # set UDM_URL / UDM_USERNAME / UDM_PASSWORD
$EDITOR config/targets.yml   # set your home-gateway IP (and ISP, if not Comcast)
docker compose up -d --build
```

Then open **http://localhost:3000** — you land straight on the **Home Internet
Latency** dashboard, no login required. That's it.

> Just want the latency graphs and don't have a UniFi gateway? See
> [Running without a UDM](#running-without-a-udm).

## What you need to do on the UniFi Dream Machine Pro

A one-time, ~2-minute setup:

1. **Create a local admin account.** In UniFi OS go to **Settings → Admins &
   Users → Add Admin**, and choose **"Restrict to local access only"**. Do *not*
   use your Ubiquiti cloud (SSO) login — local accounts are more reliable for an
   API client and keep your cloud credentials out of the container.
   - A **Viewer** role is sufficient; the exporter only issues read-only
     `GET /stat/*` calls. Give it full local admin only if you prefer.
2. **Note the gateway URL** — usually `https://192.168.1.1`. Put it, plus the
   username and password, into your `.env` file (`UDM_URL`, `UDM_USERNAME`,
   `UDM_PASSWORD`).
3. **(Optional) Schedule periodic Speed Tests.** UniFi OS → **Network →
   Settings → Internet → Speed Test**. This populates the
   `udm_speedtest_*` panels. WAN latency, throughput, CPU/memory and client
   counts are reported regardless.
4. Nothing else. The UDM presents a self-signed TLS certificate, so the exporter
   skips certificate verification by default (`UDM_INSECURE=true`). You do **not**
   need to install a cert or open any ports on the UDM.

## Network permissions & capabilities

This is the one thing that can't be hand-waved: **measuring latency requires
sending ICMP echo (ping) packets and ICMP traceroutes, which need a raw network
socket.**

- **The prober container is granted the Linux `NET_RAW` capability** in
  `docker-compose.yml`:

  ```yaml
  prober:
    cap_add:
      - NET_RAW
  ```

  This is the *minimum* privilege needed — it lets the process open ICMP sockets
  but grants nothing else. The container does **not** run `--privileged` and runs
  as an unprivileged user inside a distroless image. `NET_RAW` is a default
  Docker capability, so on most hosts this works out of the box; it's listed
  explicitly so the requirement is visible and survives hardened/default-deny
  setups.

- **`privileged: true` in `config/targets.yml`** is unrelated to Docker
  privilege — it tells the *prober* to use raw ICMP sockets (the reliable choice
  given `NET_RAW`) rather than unprivileged datagram-ICMP sockets, which
  additionally require the host sysctl `net.ipv4.ping_group_range` to be set.

- **No inbound ports are required** for probing. The only published port is
  Grafana's **3000** (`ports: ["3000:3000"]`). Prometheus (9090) and the two
  exporters (9430/9431) are reachable only on the internal Docker network.

- **Outbound:** the prober needs to reach the internet (ICMP) and your LAN
  gateway; the udm-exporter needs HTTPS to the UDM on your LAN.

### First-hop / traceroute accuracy (optional)

With the default bridge network, traceroutes show one or two extra
Docker/host hops before reaching your real gateway. The added latency is
sub-millisecond and constant, so it does **not** mask spikes — but if you want
the path to start exactly at your physical gateway, run the prober on the host
network:

```yaml
  prober:
    network_mode: host   # add this; remove its `networks:` block
```

and change the Prometheus scrape target for the `prober` job from `prober:9430`
to `host.docker.internal:9430` (or the host's LAN IP). Most users don't need
this.

### Platform notes

- **Linux:** works as described.
- **macOS / Windows (Docker Desktop):** containers run inside a Linux VM, so
  `NET_RAW` and ICMP work, but the "host" for `network_mode: host` is that VM,
  not your Mac/PC — the bridge default is recommended there.

## Configuration

### Targets — `config/targets.yml`

This file is bind-mounted into the prober, so edits take effect on
`docker compose restart prober`. Each target has a stable `name` (keep it stable
so historical graphs line up), a `host`, a `group`, and an optional `trace`
flag. Groups organize the dashboard and your reasoning:

| group     | meaning                              | what a spike here tells you            |
|-----------|--------------------------------------|----------------------------------------|
| `gateway` | your own router / first hop          | the problem is **inside your house**   |
| `isp`     | your ISP's own infrastructure        | the problem is **your ISP** (show them)|
| `anchor`  | diverse, well-run public anchors     | rules out the far end being at fault   |
| `content` | sites/services you actually use      | real-world impact                      |

The shipped defaults probe your gateway, Comcast's resolvers (`75.75.75.75` /
`75.75.76.76`), and a spread of anchors (Cloudflare, Google, Quad9, Level3).
**Edit at least `home-gateway`** to your gateway's LAN IP. If you're not on
Comcast, swap the `isp` targets for your ISP's gateway/resolver.

Timing knobs (defaults shown) at the top of the file:

```yaml
interval: 10s          # one probe cycle; pings are spread across it
pings: 20              # echo requests per target per cycle (the "smoke")
timeout: 2s            # per-reply timeout (must be < interval)
privileged: true       # raw ICMP sockets (see Network permissions)
trace_interval: 60s    # traceroutes are heavier; run them less often
trace_max_hops: 30
trace_timeout: 2s
```

### Path discovery — diverse, short routes (automatic)

Probing a dozen destinations that all leave your house the same way is mostly
redundant: when that shared segment hiccups, *everything* spikes at once and you
learn nothing about *where*. The `discovery:` block fixes this. Periodically it
traces a candidate pool, then promotes a **path-diverse** subset to active
probing — keeping the ones whose **2nd/3rd hops differ** and that **reach their
destination in the fewest hops** (closer targets localize a fault more
precisely). Selected candidates are probed and traced under the group
`discovered`; the always-on `targets` above are never touched.

```yaml
discovery:
  enabled: true
  interval: 15m         # re-trace the pool and re-select this often
  max_targets: 6        # how many diverse paths to actively probe
  max_hops: 8           # short traces during discovery (only early hops matter)
  max_reach_hops: 12    # ignore candidates farther than this
  candidates:
    - { name: cloudflare-alt, host: 1.0.0.1 }
    - { name: opendns-a,      host: 208.67.222.222 }
    # ... a broad pool spanning distinct operators/ASNs
```

Candidate names must be unique and must not collide with the always-on targets.
The dashboard's **Active discovered paths** panel shows which are currently
selected, and **Candidate distance** shows how many hops away each one is.

### Triggered diagnostics — deeper tests during a spike

Intermittent problems are usually gone before you can react. The
`diagnostics:` block watches every probe cycle and, the moment a target looks
anomalous, fires a burst of deeper tests automatically — capturing evidence
while the problem is still happening:

- a **fresh traceroute** (a path snapshot taken *during* the event);
- a **TCP handshake time** to the target — because many ISPs deprioritize ICMP,
  this proves whether *real* traffic is affected, not just pings; and
- a **DNS resolution time** — slow lookups masquerade as "the internet is slow".

A run fires when a cycle's median RTT exceeds the target's rolling baseline by
both a factor and an absolute margin (so neither tiny jitter nor a small
relative bump alone trips it), or when loss crosses a threshold — debounced by a
cooldown so a sustained event triggers once, not every cycle.

```yaml
diagnostics:
  enabled: true
  latency_factor: 3.0       # trigger when median > 3x the rolling baseline
  latency_abs_margin: 30ms  # ...and at least 30ms above it
  loss_threshold: 0.1       # or when loss >= 10%
  cooldown: 60s             # at most one run per target per minute
  baseline_alpha: 0.2       # EWMA smoothing for the "normal latency" baseline
  tcp_port: 443             # handshake port for the TCP latency test
  dns_probe: www.google.com # name resolved & timed during a run
  workers: 2                # concurrent diagnostic runs
```

Every trigger also drops a **red annotation** across the whole dashboard, so you
can line up exactly when deeper tests fired against the latency and WAN graphs.

### AI root-cause analysis — "why did this happen?" (optional)

The mechanical diagnostics gather raw signal; this turns it into an explanation.
When an event fires, a **Claude agent** investigates it and writes a
plain-language root cause onto the dashboard. It's given read-only tools and
decides how to use them:

- **query Prometheus** — to see whether the spike hit *every* target at once
  (upstream/ISP) or just one path, and whether it lined up with the WAN
  saturating (bufferbloat);
- **traceroute / DNS lookup** — to re-probe the path and resolution *now*;
- **RDAP lookup** — to name the operator/ASN that owns a slow or lossy hop (so a
  bad segment gets attributed to Comcast vs. a transit provider).

Each event gets a **monotonic Event #N** (also exported as
`diagnostic_event_id{target}`), and the agent's writeup is posted as a numbered,
tagged **Grafana annotation**. They appear as purple markers on the timeline and
in the **"AI latency diagnoses"** panel at the bottom of the dashboard — so every
spike, its diagnostic run, and its explanation line up by number.

**This is fully optional and off by default.** It activates only when you set an
API key:

```sh
# in .env
ANTHROPIC_API_KEY=sk-ant-...
```

With no key, the prober simply skips the analysis — everything else works
unchanged. Tunables live under `diagnostics.ai` in `config/targets.yml`:

```yaml
diagnostics:
  ai:
    enabled: true
    model: claude-opus-4-8   # set claude-sonnet-4-6 for cheaper, faster analysis
    max_iterations: 12       # cap on the agent's tool-use loop per event
```

Two things to know before enabling it:

- **It sends event telemetry to the Anthropic API** — your traceroute hops, IP
  addresses, and ISP identity are part of what the agent reasons over. Don't
  enable it if that's not acceptable.
- **It costs per event** (bounded by the diagnostics `cooldown`, so at most one
  analysis per target per minute). Opus is the default for best diagnosis;
  switch `model` to `claude-sonnet-4-6` to cut cost.

The agent posts annotations using the Grafana admin account
(`GRAFANA_ADMIN_PASSWORD` from `.env`) over the internal Docker network; no
ports are exposed for this.

### Secrets / Grafana — `.env`

See `.env.example`. The `.env` file holds your UDM password and is gitignored.
Grafana defaults to anonymous, login-free viewing so the stack is zero-click and
the dashboard is easy to link-share; set `GRAFANA_ANON_ENABLED=false` and
`GRAFANA_DISABLE_LOGIN=false` if you ever expose it beyond your trusted LAN.

## Reading the dashboard

The dashboard (**Home Internet Latency**) is built to be read top to bottom and
shared as-is:

1. **Overview** — at-a-glance status tiles: worst packet loss, gateway WAN
   latency, WAN up/down throughput, gateway CPU/memory. Red = bad right now.
2. **Median latency / Packet loss — all targets** — the diagnosis row:
   - Spikes on **every** target at once → upstream (your ISP / WAN).
   - Spikes on **one** target only → that specific path.
   - Loss to your **gateway** → inside the house; loss to anchors but *not* the
     gateway → the ISP.
3. **Smoke (per target)** — pick a target in the **Target** dropdown. The shaded
   band is the spread from best to worst ping in each cycle (jitter); the inner
   band is p10–p90; the bold line is the median. A wide band with a flat median
   means the connection is jittery even when "average" latency looks fine — a
   classic call-quality killer.
4. **Per-hop latency (traceroute)** — which hop is the *first* to spike owns the
   problem. Hover a hop to see the router address (useful to hand to your ISP).
5. **UniFi Dream Machine** — WAN throughput, gateway CPU/memory, gateway-reported
   latency/speedtest, client counts, all on the same time axis. If latency
   spikes line up with the WAN maxing out, that's **congestion/bufferbloat**, not
   an ISP fault — fix it with QoS/Smart Queues rather than a support ticket.
6. **Path discovery & triggered diagnostics** — which diverse paths are currently
   active, how far away each candidate is, when deeper diagnostics fired, and the
   TCP-handshake / DNS-lookup times they captured. **Red annotations** across the
   whole dashboard mark each diagnostic trigger, so you can align "deeper tests
   fired here" with the latency and WAN graphs above.
7. **AI latency diagnoses** *(if enabled)* — the Claude agent's numbered,
   plain-language root cause for each event. Purple annotations mark them on the
   timeline; the panel lists the writeups. Empty unless `ANTHROPIC_API_KEY` is set.

**Sharing with your ISP:** select the time window around an incident, take a
screenshot of the median-latency and traceroute panels (and the WAN-throughput
panel to pre-empt "you were just using it heavily"), or — since viewing is
anonymous — send them the Grafana link if they're on your network/VPN. The stack
includes the **Grafana image renderer**, so you can export clean PNGs
server-side instead of taking manual screenshots:

- **Whole dashboard** → the **"Export dashboard as PNG"** link in the dashboard's
  top-right bar renders the entire board (current time range + selected target)
  to a single PNG in a new tab; right-click → Save, or send the link.
- **A single panel** → its menu → **Share → Direct link rendered image**.

(The tall full-dashboard render uses a fixed height; if the bottom is cut off,
bump `height=` in the dashboard's link URL — `docker/grafana/dashboards/latency.json`.)

## Metrics reference

Prober (`:9430/metrics`):

| metric | meaning |
|---|---|
| `probe_up{target,group}` | 1 if the last cycle got at least one reply |
| `probe_loss_ratio{target,group}` | fraction of packets lost, last cycle |
| `probe_rtt_best/worst/median/mean/stddev_seconds` | per-cycle RTT summary |
| `probe_rtt_percentile_seconds{percentile}` | p10/p25/p75/p90 for the smoke band |
| `probe_packets_sent_total` / `probe_packets_received_total` | counters |
| `traceroute_hop_rtt_seconds{target,group,ttl}` | RTT to the router at each hop |
| `traceroute_hop_info{target,ttl,addr}` | the router address seen at each hop |
| `traceroute_path_length` / `traceroute_reached` | path length / reached dest |
| `discovery_selected{target}` | 1 if the candidate is currently promoted to active probing |
| `discovery_reach_hops{target}` / `discovery_reached{target}` | candidate distance / reachability |
| `diagnostic_triggered_total{target,reason}` | count of latency/loss-triggered diagnostic runs |
| `diagnostic_tcp_connect_seconds{target}` / `_up` | TCP handshake time / success from the last run |
| `diagnostic_dns_lookup_seconds{target}` / `_up` | DNS resolution time / success from the last run |
| `diagnostic_event_id{target}` | id of the most recent event (maps annotation ↔ run) |
| `diagnostic_ai_analyzed_total{target}` / `diagnostic_ai_failed_total{target}` | AI analyses completed / failed |

UDM exporter (`:9431/metrics`): `udm_up`, `udm_wan_latency_ms`,
`udm_wan_rx_bytes_per_second`, `udm_wan_tx_bytes_per_second`, `udm_wan_drops`,
`udm_speedtest_{download,upload}_mbps`, `udm_speedtest_ping_ms`,
`udm_gateway_cpu_percent`, `udm_gateway_memory_percent`,
`udm_gateway_uptime_seconds`, `udm_clients`.

## Running without a UDM

The latency probing is fully independent of the UniFi exporter. To run just the
probes + dashboard, comment out the `udm-exporter` service in
`docker-compose.yml` (the UDM panels will simply show "No data"), or leave it —
it will log auth failures and report `udm_up 0` without affecting anything else.

## Troubleshooting

- **All targets show `probe_up 0` / "socket: permission denied" in
  `docker compose logs prober`** → the container didn't get `NET_RAW`. Confirm
  the `cap_add: [NET_RAW]` block is present and your Docker host/policy allows
  it. As a fallback you can set `privileged: false` in `targets.yml` *and* set
  `net.ipv4.ping_group_range="0 2147483647"` on the host.
- **`udm_up 0`** → check `docker compose logs udm-exporter`. Usual causes: wrong
  `UDM_URL`, a cloud (SSO) account instead of a local one, or a wrong
  password/role.
- **Gateway target times out but the internet works** → your gateway may rate-
  limit or drop ICMP to itself; point `home-gateway` at its LAN IP and confirm it
  answers `ping`.
- **Dashboard empty for a minute after startup** → normal; Prometheus needs a
  scrape cycle or two before the first points appear.
- **"Active discovered paths" is empty** → discovery uses the same raw-ICMP
  traceroutes as everything else, so it needs `NET_RAW` (see above). It also only
  runs its first pass at startup and then every `discovery.interval`; give it a
  moment. With no `candidates` configured it stays dormant by design.

## Development

```sh
make test     # go test ./...
make vet
make build    # local binaries into ./bin
make up       # docker compose up -d --build
```

Layout: `cmd/prober` and `cmd/udmexporter` are the two binaries (one image, two
entrypoints); `internal/{config,pinger,tracer,metrics,udm}` hold the logic;
`docker/` holds Prometheus + Grafana provisioning and the dashboard JSON.
