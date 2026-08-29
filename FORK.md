# Fork notes — drewstreib/prometheus-plex-exporter

Fork of `jsclayton/prometheus-plex-exporter` (AGPL-3.0). Forked **2026-08-29** at upstream
`041ea97`. Consumer is the xcluster k3s cluster: `manifests/media/plex-exporter.yaml`, ns `media`,
house-prom job `plex`.

**Why the fork exists:** upstream publishes **amd64 only** (`:main`/`:latest`, one manifest, built
2024-08-21), so the Deployment carried `nodeSelector: kubernetes.io/arch: amd64` and pinned to the
single remaining amd64 node — no failover, and `library_duration_total` has continuity back through
the truenas migration, so any gap is permanent. This fork publishes a real amd64+arm64 manifest list.

**Upstream is dormant, not hostile:** zero commits on `main` since 2024-08-21, no tags, no releases
ever, 23 open issues / 5 open PRs unmerged (arm64 = issue #29, open since 2024-09-25). Expect to
carry our own fixes. `git fetch upstream && git log HEAD..upstream/main` is the drift check; there is
no cadence for it (a quarterly sweep would no-op indefinitely) — xcluster's `PATROL.md` ⚡ block holds
an event trigger instead.

**Public on purpose.** AGPL-3.0: publishing an image built from this source is distribution, which
obliges us to offer the corresponding source. A public fork satisfies that by construction, and keeps
the GHCR package public so the cluster needs no `imagePullSecret` for a live Deployment.

## Our delta from upstream

Additive only, except one deletion — so `git merge upstream/main` stays trivial.

- `Dockerfile` (**new**, root) — multi-stage buildx cross-compile keyed on `$TARGETARCH`. Upstream's
  `cmd/prometheus-plex-exporter/Dockerfile` + `Makefile` are **untouched** and still work.
- `.github/workflows/release.yml` (**new**) — tag `vX.Y.Z` → multi-arch build+push to GHCR.
- `.github/dependabot.yml` (**new**) — gomod + docker + github-actions, weekly. Nothing else patches
  this tree.
- `.github/workflows/publish.yml` (**deleted**) — pushed to `ghcr.io/jsclayton/*`, which our token
  cannot write; it would red-X every push to `main`. `release.yml` replaces it.
- `.github/workflows/ci.yml` (**kept as-is**) — already cross-builds linux/arm64 and runs tests.

⚠ **`vendor/` is upstream's and is UNUSED by our image** — the Dockerfile builds `-mod=mod`, because
Dependabot updates `go.mod`/`go.sum` but cannot run `go mod vendor`. Editing `vendor/` changes
nothing about the published image.

⚠ **v1.0.0 is a build-only change** — same source as upstream `041ea97`, no behaviour patches, so a
metric that moves after the cutover is the build and not us. Behaviour fixes land in later tags, one
at a time.

## Release

```
git tag v1.0.1 && git push origin v1.0.1     # CI builds both arches, pushes the tag to GHCR
```
Tags are immutable. Then bump the image in `manifests/media/plex-exporter.yaml` (Flux deploys it).

## Candidate fixes not yet taken

- upstream PR #40 — plays getting stuck (the one with real value here).
- upstream issue #37 — label inversion on `play_seconds_total`. ⚠ **Not a drive-by**: it changes
  series semantics on a metric archived to Thanos forever. Deliberate decision required.
- upstream PR #41 / issue #38 / #29 — websocket bump, configurable listen address, arm64 (ours now).
