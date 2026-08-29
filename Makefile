# Ours. Upstream's Makefile built the binary on the host and shelled out to `docker tag
# ghcr.io/jsclayton/...`; the image is now built entirely inside buildx (see ./Dockerfile),
# so this file is only a developer convenience.
#
# ⚠ There is no vendor/ any more — everything builds -mod=mod so Dependabot PRs build as-is.

BIN     := prometheus-plex-exporter
IMAGE   := ghcr.io/drewstreib/prometheus-plex-exporter
GOOS    ?= linux
GOARCH  ?= $(shell go env GOARCH)

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -mod=mod -o ./bin/$(BIN) ./cmd/$(BIN)

.PHONY: run
run:
	go run ./cmd/$(BIN)

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

# The security gate. The binary is statically linked, so stdlib CVEs ship unless the
# toolchain moves — `-mode=binary` is the only check that sees the Go version we ship.
.PHONY: vuln
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: vuln-binary
vuln-binary: build
	go run golang.org/x/vuln/cmd/govulncheck@latest -mode=binary ./bin/$(BIN)

.PHONY: check
check: vet test vuln

# Local multi-arch build sanity (does not push).
.PHONY: image
image:
	docker buildx build --platform linux/amd64,linux/arm64 -t $(IMAGE):dev .
