# Cistern

**Language:** [中文](README.zh-CN.md) | English

> Intranet download & cache hub — **fetch each artifact from the public internet once**, then share it across every laptop and CI job.
>
> **Warm the cache while online, keep installing when offline** — built for capped egress and air-gapped windows.
>
> Designed for constrained egress: **smart routing · parallel chunking · interactive-first scheduling · one-box ops**

**PyPI, npm, Docker, Go modules, Hugging Face, and Maven/Gradle** share one download + cache + rate plane (enable per module).

![Admin dashboard](docs/pic/UI-EN.png)

---

## Why teams reach for Cistern

Corporate networks usually fail installs the same way:

- Egress is capped; `torch`, CUDA wheels, and fat wheels crawl for tens of minutes on a single connection
- The same multi-GB blob is pulled again by the next developer, the next CI matrix cell, the next night rebuild
- Everyone configures their own proxy and retries — pain multiplies per machine, with **no shared on-disk cache**

Cistern is the shared choke point that **pulls once, caches locally, and serves the LAN at full speed**.  
Prefetch (or install once) while you have egress; **after that, clients can keep installing through the same local `index-url` with the upstream offline** — drills, isolated labs, shipboard / OT networks.  
Need international egress? Set one **upstream proxy (HTTP/SOCKS)** on the server — not on every laptop.

---

## Advantages & design

Cistern is not “another PyPI mirror.” It is an **ops-shaped download plane** built around how intranet teams actually install software.

### 1. Two ports, clear responsibilities

| Plane | Default | Job |
|-------|---------|-----|
| **Download service** | `:18081` | What `pip` / `uv` talk to — indexes, packages, cache hits |
| **Admin** | `:18082` | Dashboard, queue, prefetch, settings — keep ops off the hot path |

Clients never need the admin port. Operators never confuse “proxy for users” with “panel for admins.”

### 2. Smart path: right strategy per object

- **Indexes & small metadata** → lightweight proxy + rewrite (fast, cache-friendly)
- **Large artifacts** → **HTTP Range parallel chunks** (saturate the allowed egress, then stick locally)
- **Index links** rewrite to *this* download host automatically from the request **Host** — LAN machines just point at Cistern; no brittle hard-coded public URL in most setups

### 3. Interactive-first scheduling (design center) 👑

Bandwidth and concurrency are scheduled like an ops product — **never a dumb pipe**:

| Class | Role | Behavior |
|-------|------|----------|
| 🚨 **P0** Interactive | Someone is waiting on `pip install` | **Zero shaping** — human waits always cut the line |
| ♻️ **P1** Resume | Prefetch paused mid-flight | Soft-pause / resume — **don’t throw the whole download away** |
| 🌙 **P2** Prefetch | Background warm-up | Yields under load; windowed limits squeeze here |

Daytime installs feel **LAN-fast**; nights and idle windows **stuff the cache to the brim**.

### 4. Prefetch that respects the link 🔥

Paste requirements / `pyproject` lines → resolve the closure → **flood the cache before peak hour**.  
Prefetch is first-class in the UI: live queue, progress, batch cancel — **see the backfill, steer it, never blind-pull**.

Queue **🧹 Clear** menu — three blast radii, zero ambiguity:

| Level | Job | Blast radius |
|-------|-----|--------------|
| 🧾 **Clear finished** | Sweep history noise | done / error / cancelled only |
| 🎯 **Clear prefetch** | Stop background warm-up | **P1/P2 only** — interactive **P0 stays untouched** |
| 💥 **Clear all** | Nuke the battlefield | **Includes P0** — even in-flight installs get cancelled |

### 5. Download first, use offline later

Typical flow: **prefetch (or install once) online → artifacts on disk → keep using the same `index-url` with no upstream**.

- **Wheels / sdists** serve as local HITs with no outbound calls  
- **Indexes / metadata** that expire while upstream is down fall back to the last cached copy (`X-Cache: STALE`) so installs don’t die on a dead link  
- Anything not cached still needs the network — warm the target platforms and dependency closure via prefetch before you go offline

### 6. One container, zero client sprawl

- Single image: Go binary + embedded admin UI + SQLite (or Postgres if you prefer)
- Data on a volume (`/data`) — **update the image, keep the cache**
- **Public setup guide** at `/` (no login; default entry): pick a local download address, copy client snippets

### 7. Multi-source, one ops model

Platforms are **modules** (enable, upstreams, download knobs). PyPI, npm, Docker, Go, Hugging Face, and Maven plug into the **same** download + cache + rate plane — one habit for the whole org.

```text
  Dev / CI                              Public upstream
      │                                       ▲
      ▼                                       │
  ┌─ Cistern ─────────────────┐            │
  │ Download ──► cache HIT?     │            │
  │               │ miss         │            │
  │               ▼              │            │
  │          chunked fetch ─opt─► 🌐 upstream proxy ─┘
  │ Admin: guide · queue · limits│
  └──────────────────────────────┘
```

---

## Feature map

| | Capability | Why it matters |
|--|------------|----------------|
| ⚡ | Parallel chunks | Large wheels stop wasting a single TCP stream |
| 💾 | Local cache + TTL / size caps | Second install is LAN-speed |
| 📴 | Download first, offline later | Warm the closure; install with upstream down |
| 🎯 | Strategy routing | Indexes stay light; payloads go wide |
| 📝 | Host-aware index rewrite | Clients use the address they already hit |
| 🌐 | Server-side upstream proxy | Configure egress **once** |
| 🎚️ | Windowed rate limits | Protect the shared pipe; don’t punish interactive installs |
| 🔥 | Prefetch + live queue UX | Progress you can trust; 🧹 three clears (finished / prefetch / 💥incl. P0) |
| 📊 | Admin UI | Capacity, hits, P0/P1, packages, settings — **one panel to rule them** |
| 🚀 | Online upgrade & restart | GitHub one-click update; ⚠️ danger-zone hot restart |
| 🧩 | Modular platforms | PyPI / npm / Docker / Go / HF / Maven — **one ops muscle memory** |

---

## vs [devpi](https://devpi.net/)

| | Cistern | devpi |
|--|:---------:|:-----:|
| Role | Unified **downloader / multi-source cache** for the LAN | PyPI mirror + **private indexes & publish** |
| Avoid repeat downloads | ✅ | ✅ |
| Chunking · interactive-first limits · prefetch · ops UI | ✅ | — |
| Upstream proxy (server egress) | ✅ | Depends on deploy |
| Private upload · index inheritance | — | ✅ |

```text
Cistern: client ──► download svc ──► cache ──► upstream (optional proxy)
devpi:     client ──► index tree ─┬─► private packages
                                 └─► root/pypi mirror
```

Use **Cistern** when the pain is egress and repeated pulls.  
Use **devpi** when you need a private package warehouse. Many teams run both.

---

## Architecture

```mermaid
flowchart LR
  subgraph clients [Clients]
    PIP[pip / uv]
    NPM[npm]
    DOCKER[docker]
    GO[go]
    HF[huggingface-cli]
    MVN[mvn / gradle]
  end

  subgraph mh [Cistern]
    DL[Download service]
    ADM[Admin]
    CACHE[Local cache]
    RATE[Rate / scheduler]
  end

  subgraph out [Egress]
    UP[Upstream proxy optional]
  end

  subgraph remote [Public upstream]
    PYPI[PyPI]
    NPMREG[npm]
    DREG[Docker Hub]
    GOMOD[Go proxy]
    HFHUB[HuggingFace]
    MAVEN[Maven]
  end

  PIP --> DL
  NPM --> DL
  DOCKER --> DL
  GO --> DL
  HF --> DL
  MVN --> DL
  ADM -.-> DL
  DL --> CACHE
  CACHE -->|MISS| RATE
  RATE --> UP
  UP --> PYPI
  UP --> NPMREG
  UP --> DREG
  UP --> GOMOD
  UP --> HFHUB
  UP --> MAVEN
  RATE -->|direct| PYPI
  CACHE -->|HIT| clients
```

### Data layout

Root: `CISTERN_DATA_DIR` (default `./data`; Compose usually `/data`). Ops settings live in SQLite (`cistern.db`) or Postgres via `DATABASE_URL`.

```text
data/
├── cistern.db            # config, admin user, light stats (if not using Postgres)
├── logs/
└── cache/
    ├── meta.db           # cache entry metadata (bbolt)
    ├── digests.json      # expected digests
    ├── catalog/          # e.g. PyPI package-name catalog
    ├── objects/
    │   ├── pypi/ab/…     # artifacts partitioned by platform
    │   ├── npm/…
    │   ├── docker/…
    │   └── …
    └── tmp_*.part        # in-flight downloads
```

On startup, legacy `objects/{ab}/{hash}` trees are **migrated** into `objects/{platform}/…` and empty shard dirs are pruned. Capacity limits remain **global** (`max_size_gb`), not per-platform quotas.

---

## Quick start

### 1. Docker Compose (recommended)

```bash
cd deploy
docker compose up -d
```

| URL | Role |
|-----|------|
| http://localhost:18081 | Download service (`pip -i` / `uv`) |
| http://localhost:18082 | Admin UI (default `admin` / `admin`; opens on the guide) |
| http://localhost:18082/login | Login |

Image: `ghcr.io/nihaoyanzu/cistern:latest`. Local build: `docker compose up -d --build`.

### 2. First-time setup

Defaults are usually enough:

```text
Guide http://localhost:18082/ (no login)
  · Builds download URLs from the Host you used to open the page
  · Index links rewrite from request Host — no hard-coded public URL to fill in

Admin login: admin / admin (seeded on first boot; change later under Account)

System → Network
  · Upstream proxy → empty (direct) or socks5://127.0.0.1:1080 if required

Platforms → PyPI
  · Enabled: yes
  · Index / file upstream → https://mirrors.aliyun.com/pypi (or your mirror)
```

> **Upstream proxy** = Cistern’s own path to the public internet.  
> It is **not** each laptop’s `HTTP_PROXY`, and **not** the download service URL.

### 3. Client: PyPI

```bash
# one-off
pip install torch -i http://localhost:18081/simple/ --trusted-host localhost

# persistent
pip config set global.index-url http://localhost:18081/simple/
pip config set global.trusted-host localhost

# uv
uv pip install torch -i http://localhost:18081/simple/
```

```text
pip ──► /simple/     index (links rewritten to the download service)
     ──► /packages/  wheel / sdist
            │
       HIT ✅ local    MISS ❌ backfill (optional upstream proxy) → cache
```

✨ Prefetch, live queues, package search — all in the admin UI; **🧹 Clear** on the queue page nukes by scope (finished / prefetch-only / 💥 incl. P0).  
🎚️ Rate limits: **System → pull rate** (bandwidth shaping hits prefetch only; **interactive P0 stays full-speed**).  
🚀 Online upgrade + **Restart service** → **System → ⚠️ Danger zone** (blocked mid-upgrade so you don’t half-swap a running binary).

### 4. Other clients

| Client | Status | Config |
|--------|:------:|--------|
| pip / uv | ✅ | `-i http://localhost:18081/simple/` |
| npm | ✅ | `npm config set registry http://localhost:18081/` |
| Docker | ✅ | `registry-mirrors` → `http://localhost:18081` |
| Go modules | ✅ | `export GOPROXY=http://localhost:18081,direct` |
| HuggingFace | ✅ | `export HF_ENDPOINT=http://localhost:18081` + `HF_HUB_DISABLE_XET=1` |
| Maven / Gradle | ✅ | `settings.xml` mirror or Gradle `maven { url }` → `http://localhost:18081/` |
| R / CRAN | ☐ | `options(repos = c(CRAN = "http://localhost:18081/"))` |
