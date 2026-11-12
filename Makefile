GO ?= go
GIT ?= git
GOLANGCI_LINT ?= golangci-lint
HELM ?= helm
# Whatever builds a Dockerfile: docker, nerdctl, podman.
CONTAINER_TOOL ?= docker

TARGET_BIN := blockoverlayfs
PKG := github.com/YiD11/blockoverlayfs
VERSION ?= $(shell $(GIT) describe --match 'v[0-9]*' --dirty='.m' --always --tags 2>/dev/null)
REVISION ?= $(shell $(GIT) rev-parse HEAD 2>/dev/null)$(shell if ! $(GIT) diff --no-ext-diff --quiet --exit-code 2>/dev/null; then echo .m; fi)

# The plugin only runs on Linux, so binaries cross-compile by default; tests use the host platform.
GOOS ?= linux
GOARCH ?= $(shell $(GO) env GOARCH)
GO_LDFLAGS := -s -w -X $(PKG)/version.Version=$(VERSION) -X $(PKG)/version.Revision=$(REVISION)
GO_BUILD := CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags "$(GO_LDFLAGS)"

IMAGE ?= $(TARGET_BIN):$(VERSION)
CHART := charts/$(TARGET_BIN)

.PHONY: all binaries image test test-root vet lint helm-lint tidy clean help

all: binaries

help:
	@echo "Usage: make <target>"
	@echo
	@echo " * 'binaries'  - Build bin/$(TARGET_BIN) for GOOS=$(GOOS) GOARCH=$(GOARCH)."
	@echo " * 'image'     - Build the container image $(IMAGE) with $(CONTAINER_TOOL)."
	@echo " * 'test'      - Run unit tests on the host platform."
	@echo " * 'test-root' - Run the block layer tests on loop devices, ext4 and overlayfs (Linux, root)."
	@echo " * 'vet'       - Run go vet for the host platform and for linux."
	@echo " * 'lint'      - Run golangci-lint."
	@echo " * 'helm-lint' - Lint and render the Helm chart."
	@echo " * 'tidy'      - Run go mod tidy."
	@echo " * 'clean'     - Remove build artifacts."

binaries:
	$(GO_BUILD) -o bin/$(TARGET_BIN) ./cmd/$(TARGET_BIN)

# The image is built for the platform of the builder; pass IMAGE to name it for your registry.
image:
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) -t $(IMAGE) .

test:
	$(GO) test -gcflags="all=-N -l" ./...

# Only pkg/block defines -test.root. The race detector needs cgo; set ROOT_TESTFLAGS= to run without it.
ROOT_TESTFLAGS ?= -race
test-root:
	$(GO) test $(ROOT_TESTFLAGS) -count=1 ./pkg/block/ -test.root

vet:
	$(GO) vet ./...
	GOOS=linux $(GO) vet ./...

# Linted as linux: off Linux the block layer is a stub that always fails, which staticcheck reports as unreachable
# code in main.
lint:
	GOOS=linux $(GOLANGCI_LINT) run ./...

helm-lint:
	$(HELM) lint $(CHART)
	$(HELM) template $(TARGET_BIN) $(CHART) --namespace kube-system > /dev/null

tidy:
	$(GO) mod tidy

clean:
	$(RM) -r bin _output
