# FORK.md — this is a hard fork, independently maintained

Began as `jsclayton/prometheus-plex-exporter` (AGPL-3.0), forked 2026-08-29 at upstream `041ea97`.
**Severed 2026-08-29 (Drew): we are the maintenance path.** The module is self-referencing, upstream
compatibility is explicitly a non-goal, and there is no expectation of merging from or to upstream.
Consumer is the xcluster k3s cluster — `manifests/media/plex-exporter.yaml`, ns `media`, house-prom
job `plex`.

**Why it exists:** upstream publishes **amd64 only** (`:main`/`:latest`, one manifest, built
2024-08-21) and is dormant — zero commits on `main` in two years, no tags or releases ever, 23 open
issues and 5 open PRs unmerged. ⚠ Its `pushed_at` field reads recent and means nothing (a dependabot
branch); don't read liveness off it.

## Severance — what "self-referencing" means here

- **Module path is `github.com/drewstreib/prometheus-plex-exporter`** (was `github.com/grafana/plexporter`).
- **`vendor/` is deleted and gitignored.** Everything builds `-mod=mod`. ⚠ This was the deciding
  factor: Dependabot updates `go.mod`/`go.sum` but **cannot run `go mod vendor`**, so a vendored tree
  makes every dependency PR either stale or a build failure.
- **Our own `Makefile`, `ci.yml`, `release.yml`, `Dockerfile`.** Upstream's `publish.yml`,
  `release-drafter.yml`, `tools/image-tag`, `.air.toml` and `cmd/prometheus-plex-exporter/Dockerfile`
  are deleted — they served upstream's release flow and pushed to `ghcr.io/jsclayton/*`.
- **Public on purpose.** AGPL-3.0 makes publishing an image a distribution that obliges a source
  offer; a public fork satisfies that by construction, and keeps the GHCR package public so a live
  Deployment needs no `imagePullSecret`. ⚠ Do not flip it private.

## The security posture — read before touching the build

⚠ **The Go toolchain version is a SECURITY CONTROL, not a compatibility knob.** The binary is
statically linked (`CGO_ENABLED=0`), so every `net/http`, `crypto/tls` and `crypto/x509` fix reaches
production **only by rebuilding on a newer Go**. Upstream sat on go1.23, two majors past EOL.

Measured 2026-08-29 with `govulncheck -mode=binary` against the actual shipped artifact:

| build | reachable vulnerabilities |
|---|---|
| v1.0.0 (go1.23.12, websocket 1.5.0) | **27** — 26 stdlib, 1 module |
| v1.1.0 (go1.27.0, websocket 1.5.3) | **0** |

⚠ **Source-mode `govulncheck ./...` alone would NEVER have shown 26 of those** — it reports module
vulns, not the stdlib you linked. `-mode=binary` on the built artifact is the only check that sees
the toolchain. Both run in CI (`vuln` job), plus a weekly cron so newly-disclosed CVEs surface in
code that has not changed. `go.mod` declares `go 1.27.0`, so an older toolchain refuses to build.

## Our fixes beyond upstream

- **Multi-arch** `linux/amd64` + `linux/arm64` (the reason the fork exists).
- **Runs as non-root** (`USER 65532`). Upstream's image runs as root; nothing here needs privilege.
  ⚠ The Deployment sets `runAsNonRoot`, so dropping the `USER` line fails the pod loudly by design.
- **Panic-on-scrape fixes.** Upstream indexes `Media[0]`, `Media[0].Part[0]` and
  `MediaContainer.Metadata[0]` unguarded. Plex genuinely returns sessions without them.
  ⚠ **These were fatal, not 500s:** client_golang has no `recover()` anywhere, and `Gather` runs
  `Collect` on its own goroutine — so one such session killed the exporter on the next scrape.
- **Bounded HTTP client:** a 10s timeout (upstream has none, so a hung Plex blocks the 5s refresh
  goroutine forever) and a 16 MiB response cap (upstream `io.ReadAll`s the body unbounded).
- **Startup backfill of in-flight sessions** (v3.0.0). `Update` stamps `playStarted` only on a
  transition *into* playing, so a session already in flight when the process starts never gets one —
  and a pod roll happens on every image bump. Playing-at-start was undercounted (time restarted from
  zero); **paused-at-start was dropped entirely**, because `playStarted` stayed zero and `Collect`
  skips a zero `playStarted` outright. `plexListener.backfillSessions` now reads `GetSessions()` once
  before subscribing and calls `sessions.Seed`, backdating `playStarted` by the session's
  `ViewOffset` (banking it in `prevPlayedTime` instead when not playing, so pausing cannot count it
  twice). ⚠ **Seeding is STARTUP-ONLY and must stay that way** — the obvious generalisation, seeding
  on first sight of any unknown session key, is wrong: **Plex assigns a NEW `SessionKey` to a
  resume**, so unknown keys routinely arrive mid-file (measured 2026-08-29 in Loki: key 1308 paused
  at 3m11s, key 1310 then appeared *playing* at that same 3m11s) and per-key seeding would credit the
  whole resume offset as play time on **every resume**. At startup that same offset is the only
  elapsed-time signal there is and the error is one-shot.

## Release

```
git tag v1.1.1 && git push origin v1.1.1     # CI builds both arches, pushes to GHCR
```
Tags are immutable. Then bump `tag@digest` in `manifests/media/plex-exporter.yaml`; Flux deploys it.

## ⚠ BREAKING metric changes

**`plays_total` and `play_seconds_total` changed shape twice on 2026-08-29**, and old series in
Thanos keep their old meanings. Drew's call on the first round, which governs both: *"fine to just
fix both. i don't care about past broken metrics."*

### v3.0.0 — the counters are cumulative

⚠ **They were rebuilt from the LIVE session map on every scrape**, so a series *vanished* when its
session was pruned a minute after stopping. That contradicts both the `_total` suffix and the
`CounterValue` type: Prometheus reads a disappear/reappear as a counter reset and `increase()`
re-adds the whole value. `plays_total` in particular was really a gauge — the count of *concurrent*
sessions on a tuple.

`sessions.completed` now banks a session's totals in `retire()` before it leaves the map, and
`Collect` emits banked + live. So `plays_total` is the cumulative **number of plays** this process
has seen on a tuple, and `play_seconds_total` their summed watch time.
- ⚠ **`completed` is never pruned — that is what makes the counter monotonic.** It grows with the
  number of distinct tuples the process has seen (~900 over 90d historically; ~1–2 MB, against an
  11Mi RSS and a 128Mi limit), and the scrape payload grows with it. That is the cost of the
  guarantee, not a leak.
- **Credit stops at `lastUpdate`, never `time.Now()`** (`session.observedPlayTime`). Plex sends a
  notification every ~10s, so time past the last one is unobserved. This is what lets `retire()` bank
  exactly the value `Collect` was already emitting — crediting to now instead would make the counter
  step *backwards* on a prune, which is the very thing this fixes.

### v3.0.0 — sessions that never stop are retired

⚠ **`pruneOldSessions` only ever evicted `stopped` sessions.** A client that vanishes — network drop,
app killed — sends no stop, so its session was immortal, and a still-`playing` one kept accruing play
time for a stream that ended hours ago. Anything idle past `sessionStaleTimeout` (5 min, ~30 missed
notifications, so a brief websocket reconnect cannot trip it) is now retired too. Explicitly-stopped
sessions keep the 1-minute `sessionTimeout`.
⚠ `retire()` must also flatten a still-playing session's estimated bytes: those are otherwise banked
only by `Update`'s playing→not-playing transition, which such a session never makes.

### v2.0.0 — rotated library labels, and the `session` label dropped

1. **Library labels were ROTATED three ways** — `library` held the id, `library_id` the type,
   `library_type` the name. `library_duration_total` was always correct, so the two metrics could not
   be joined on library labels. ⚠ Upstream issue #37 calls this a two-way inversion of
   `library`/`library_id` — **that is wrong; read `metrics.Play`'s argument order against
   `playLabels`, not the issue.** Guarded now by `TestCollectLibraryLabelsAreNotRotated`.
2. **The `session` label is GONE.** It bought nothing analytically and only ~34% of the cardinality
   (1,413 → 935 distinct series over 90d; the real driver is per-episode `grandchild_title`, which is
   the point of the metric). ⚠ **Dropping it is NOT just deleting the label** — it was making every
   series unique by construction. Two sessions sharing a tuple (same user restarting the same episode
   while the stopped one is inside the 1-minute prune window) now collide, and **client_golang fails
   the ENTIRE scrape on a duplicate label set**, not just the offending metric. `Collect` therefore
   folds sessions into a `playKey` map (it emitted a hardcoded `1.0` per session before). Guarded by
   `TestCollectAggregatesSessionsSharingATuple`.

## Known, deliberately not changed

- **Dependency `replace` → `github.com/jsclayton/go-plex-client`**, a 2023 pseudo-version of the
  abandoned `jrudio/go-plex-client`. Dependabot handles a pseudo-version `replace` poorly; this is
  the least-maintained thing in the tree.
- `stream_bitrate` remains high-churn (685 distinct values in 90d) — the next cardinality lever if
  one is ever wanted, though it is far behind per-episode titles.

## Tests

Upstream shipped none. `pkg/plex/sessions_test.go` covers the two v2.0.0 changes, the
panic-on-scrape guards, and v3.0.0's startup backfill, cumulative counters and stale-session
retirement. It registers the collector with a real `prometheus.Registry` and calls `Gather()` — so a
duplicate label set fails the test exactly as it would fail a scrape.
⚠ **Each test was controlled by reverting its fix and confirming it fails**; a green suite on its own
proves the test ran, not that it checks the right thing.
