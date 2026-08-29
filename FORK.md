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
- **Stderr-only logging** (v3.1.0). `silenceClientChatter` in `main.go` points `os.Stdout` at
  `/dev/null` unless `PLEX_CLIENT_DEBUG` is set. The plex client logs with unconditional
  `fmt.Print*` to stdout and offers **no toggle**: it dumps every websocket message raw, plus a line
  per unknown event type and per unmarshal failure. Measured in Loki 2026-08-28: **163,405 of the
  pod's 166,207 lines (98.3%)** were that chatter against 2,800 of ours, making the exporter the
  cluster's 4th-heaviest log producer at ~110k lines/day into a **shared 10-day** store.
  ⚠ **Nothing is lost:** all 20 of the client's stdout writes are debug chatter or an error it also
  returns / hands to the `SubscribeToNotifications` callback, and we log those on stderr ourselves.
  ⚠ **`PLEX_CLIENT_DEBUG` is the only way to see the raw notification stream** — set it on the
  Deployment, don't rebuild. ⚠ The two spam shapes differ per client and swapping the dependency
  does not fix it: the old jsclayton client emitted `unknown websocket event name` (a small event
  map) and **zero** raw dumps; jrudio v2.0.0 knows more event types but added
  `fmt.Printf("%s\n", message)` on **every** message.

## The plex-client dependency — ⚠ the `replace` is GONE (2026-08-29), don't reinstate it

We track **`github.com/jrudio/go-plex-client/v2 v2.0.0`** directly. Until 2026-08-29 `go.mod` carried a
`replace` → `jsclayton/go-plex-client`, a 2023 pseudo-version, for
[jrudio#56](https://github.com/jrudio/go-plex-client/pull/56) (`cannot unmarshal array into ... rating
of type float64`).

⚠ **The premise of that `replace` was wrong, and this file asserted the wrong thing for a day:** it
called `jrudio/go-plex-client` *abandoned*. It is not — commits through 2026-06-10 and a **`v2.0.0` tag
cut 2026-06-09**, its first ever. PR #56 was closed unmerged, but v2.0.0 fixes that exact bug properly
with a `FlexibleFloat64` type and a custom `UnmarshalJSON`. ⚠ **Don't re-derive "abandoned" from a
quiet-looking repo — check commits and tags** (`gh api repos/OWNER/REPO/commits`, `/tags`).

Measured before the move, all on the real artifact:
- Import path only (3 files); **all tests pass unchanged**, `govulncheck -mode=binary` stays **0**.
- Module graph **133 → 110**: `go mod tidy` prunes `badger/v3`, `ristretto`, `urfave/cli`,
  `go-homedir` — reachable only via the client's CLI/store code we never call. Net new require:
  `golang-jwt/jwt/v5`. Binary +1.2%.
- ⚠ **Tests could NOT have caught the real risk** — they build `plex.Metadata` from struct literals, so
  a changed JSON tag passes tests and silently yields zero values against live Plex. All **17** fields
  we read off real Plex JSON (`LibrarySectionID`, `Type`, `Title`, `GrandparentTitle`, `ParentTitle`,
  `Index`, `ParentIndex`, `ViewOffset`, `SessionKey`, `RatingKey`, `Bitrate`, `VideoResolution`,
  `Decision`, `Player.Device`, `Player.Product`, `Player.State`, `Duration`) were diffed
  byte-identical old-vs-v2. v2's `FlexibleFloat64`/`Ratings` reshaping touches only fields we never read.

⚠ **A pin freezes the library's own code, NOT its dependencies** — Go MVS means our `go.mod` wins
(the old pin asked for `gorilla/websocket v1.4.0` while we shipped v1.5.3). What the pin really cost
was owning ~2,750 frozen lines with no upstream to take a fix from.

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

## The websocket does NOT reconnect — the pod restart IS the reconnect

Settles upstream's unanswered `TODO - Does this automatically reconnect on websocket failure?`
(2026-08-29). **It does not**, and the exporter deliberately does **not** paper over that.

- `SubscribeToNotifications` dials **once**. On a read error it invokes the error callback and
  unwinds; there is no retry loop in the client or in `Listen`.
- **The failure is loud, not silent.** `onError` → `doneChan` → `Listen` returns → `main` exits →
  Deployment restarts the pod → the startup backfill re-seeds in-flight sessions. So the feared
  shape — `up{job="plex"}=1` while play metrics are frozen — is **not** how this fails, and no
  staleness alert is warranted. Guarded by `TestListenReturnsOnAbnormalWebsocketClose`: if `Listen`
  ever starts *hanging* instead of returning, that assumption is dead and the test says so.
  ⚠ A server-initiated `close(1000)` is filtered out of `doneChan` by `onError`, so `Listen` returns
  **nil** and the process exits **0** — same restart, different exit code.
- **Never observed in the field:** `changes(process_start_time_seconds{job="plex"}[1d])` over Thanos
  shows zero unexplained process starts 2026-07-31 → 2026-08-28; every start in that window is a
  known deploy or node event. ⚠ That metric **cannot** distinguish "never died" from "died silently
  and kept running" — the tests are what close that gap, not the restart count.
- **Residual, accepted:** the client sets no read deadline and no ping/pong, so a black-holed TCP
  connection blocks `ReadMessage` indefinitely. The client's 1s keepalive write bounds it to Linux's
  retransmit timeout (~15 min), after which — **code-read, never reproduced** — the ticker branch
  calls the callback with no `return`, so the next tick sends on an already-closed `doneChan` and
  **panics**. Still a crash, still a restart, just an ugly one. Fixing it properly means owning the
  subscribe loop (gorilla is already a direct dependency); declined 2026-08-29 as not worth the code.

## Known, deliberately not changed

- `stream_bitrate` remains high-churn (685 distinct values in 90d) — the next cardinality lever if
  one is ever wanted, though it is far behind per-episode titles.

## Tests

Upstream shipped none. `pkg/plex/sessions_test.go` covers the two v2.0.0 changes, the
panic-on-scrape guards, and v3.0.0's startup backfill, cumulative counters and stale-session
retirement. `pkg/plex/listener_test.go` drives a fake Plex that slams the
websocket and pins that `Listen` returns rather than hangs; `cmd/.../main_test.go` re-execs itself
to prove the client's `fmt.Println` reaches nowhere while our stderr survives. It registers the collector with a real `prometheus.Registry` and calls `Gather()` — so a
duplicate label set fails the test exactly as it would fail a scrape.
⚠ **Each test was controlled by reverting its fix and confirming it fails**; a green suite on its own
proves the test ran, not that it checks the right thing.
