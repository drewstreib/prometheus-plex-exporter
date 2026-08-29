# syntax=docker/dockerfile:1
#
# Multi-arch (linux/amd64 + linux/arm64) image -> ghcr.io/drewstreib/prometheus-plex-exporter.
#
# Why this file exists: upstream builds the binary ON THE CI HOST (`make exe`) and COPYs it into
# cmd/prometheus-plex-exporter/Dockerfile, so the published image is always whatever arch the runner
# was -- amd64, forever. This cross-compiles INSIDE buildx off $TARGETARCH, so one
# `docker buildx build --platform linux/amd64,linux/arm64` emits a real manifest list.
# ⚠ Upstream files are untouched by design (this is additive) -- `make docker-images` still works
# exactly as it did, and `git diff upstream/main` stays merge-trivial.
#
# ⚠ Builds with -mod=mod, NOT the checked-in vendor/. Dependabot updates go.mod+go.sum but cannot run
# `go mod vendor`, so a vendored build would silently go stale on every bump PR. vendor/ is upstream's
# and is UNUSED here -- editing it changes nothing about this image.
#
# ⚠ The BUILDER TOOLCHAIN IS A SECURITY CONTROL, not a compatibility knob. This binary is statically
# linked, so every net/http, crypto/tls and crypto/x509 fix ships only by REBUILDING ON A NEWER GO.
# Upstream's go1.23 was two majors past EOL: govulncheck -mode=binary on the shipped v1.0.0 artifact
# found 27 reachable vulns, 26 of them stdlib. On go1.27 + websocket 1.5.3 the same scan finds ZERO.
# ⇒ Keep this at the current Go release; Dependabot proposes the bump, the quarterly patrol enforces it.
#
# ⚠ The Makefile's `-X main.Version/Revision/Branch` ldflags are dropped: no such vars exist in
# cmd/prometheus-plex-exporter/main.go, and the Go linker ignores -X for a missing symbol. They were
# always no-ops; omitting them changes no bytes of behaviour.

FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -mod=mod -o /out/prometheus-plex-exporter ./cmd/prometheus-plex-exporter

FROM alpine:3.24
RUN apk --no-cache add ca-certificates tzdata \
 && adduser -D -H -u 65532 -s /sbin/nologin nonroot
COPY --from=build /out/prometheus-plex-exporter /prometheus-plex-exporter
# ⚠ Upstream's image runs as ROOT (it never sets USER). Nothing here needs privilege: the binary is
# static, reads two env vars, makes outbound HTTP and binds :9000 (>1024, so no CAP_NET_BIND_SERVICE).
# Pairs with the Deployment's securityContext in manifests/media/plex-exporter.yaml -- that manifest
# sets runAsNonRoot, which will REFUSE TO START if this line is ever dropped. That is the intended
# failure mode: loud, not silent.
USER 65532:65532
EXPOSE 9000
ENTRYPOINT ["/prometheus-plex-exporter"]
