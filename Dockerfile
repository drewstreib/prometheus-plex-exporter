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
# ⚠ The Makefile's `-X main.Version/Revision/Branch` ldflags are dropped: no such vars exist in
# cmd/prometheus-plex-exporter/main.go, and the Go linker ignores -X for a missing symbol. They were
# always no-ops; omitting them changes no bytes of behaviour.

FROM --platform=$BUILDPLATFORM golang:1.23.12-alpine3.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
      go build -mod=mod -o /out/prometheus-plex-exporter ./cmd/prometheus-plex-exporter

FROM alpine:3.22
RUN apk --no-cache add ca-certificates tzdata
COPY --from=build /out/prometheus-plex-exporter /prometheus-plex-exporter
EXPOSE 9000
ENTRYPOINT ["/prometheus-plex-exporter"]
