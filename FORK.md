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

## Release

```
git tag v1.1.1 && git push origin v1.1.1     # CI builds both arches, pushes to GHCR
```
Tags are immutable. Then bump `tag@digest` in `manifests/media/plex-exporter.yaml`; Flux deploys it.

## Known, deliberately not changed

- ⚠ **`play_seconds_total` / `plays_total` carry a three-way ROTATION of the library labels** —
  `library` holds the id, `library_id` holds the type, `library_type` holds the name. Verified
  against our own archived data, and `library_duration_total` is correct, so **the two metrics cannot
  be joined on library labels.** (Upstream issue #37 calls this a two-way inversion; that is wrong.)
  Fixing it rewrites the meaning of series already in Thanos forever — a deliberate decision, not a
  drive-by.
- ⚠ **`session` is a metric label**, so every playback creates two permanently-retained series:
  **1,044 distinct session values in 90 days** (~2,100 series, ~11.6/day) archived forever.
  `stream_bitrate` is similarly high-churn (685 distinct values).
- **Dependency `replace` → `github.com/jsclayton/go-plex-client`**, a 2023 pseudo-version of the
  abandoned `jrudio/go-plex-client`. Dependabot handles a pseudo-version `replace` poorly; this is
  the least-maintained thing in the tree.
- **There are no tests at all** (upstream shipped none), so `go test ./...` is a no-op that passes.
