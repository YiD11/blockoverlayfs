DESTDIR ?= /usr/local
BINDIR ?= $(DESTDIR)/bin

GO ?= go
GIT ?= git
INSTALL ?= install
GOLANGCI_LINT ?= golangci-lint

TARGET_BIN := blockoverlayfs
PKG := github.com/YiD11/blockoverlayfs
VERSION ?= $(shell $(GIT) describe --match 'v[0-9]*' --dirty='.m' --always --tags 2>/dev/null)
REVISION ?= $(shell $(GIT) rev-parse HEAD 2>/dev/null)$(shell if ! $(GIT) diff --no-ext-diff --quiet --exit-code 2>/dev/null; then echo .m; fi)

# The plugin only runs on Linux, so binaries cross-compile by default; tests use the host platform.
GOOS ?= linux
GOARCH ?= $(shell $(GO) env GOARCH)
GO_LDFLAGS := -s -w -X $(PKG)/version.Version=$(VERSION) -X $(PKG)/version.Revision=$(REVISION)
GO_BUILD := CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags "$(GO_LDFLAGS)"

.PHONY: all binaries install uninstall test test-root vet lint tidy clean help

all: binaries

help:
	@echo "Usage: make <target>"
	@echo
	@echo " * 'binaries'  - Build bin/$(TARGET_BIN) for GOOS=$(GOOS) GOARCH=$(GOARCH)."
	@echo " * 'install'   - Install bin/$(TARGET_BIN) to $(BINDIR)."
	@echo " * 'uninstall' - Remove $(BINDIR)/$(TARGET_BIN)."
	@echo " * 'test'      - Run unit tests on the host platform."
	@echo " * 'test-root' - Run the block layer tests on loop devices, ext4 and overlayfs (Linux, root)."
	@echo " * 'vet'       - Run go vet for the host platform and for linux."
	@echo " * 'lint'      - Run golangci-lint."
	@echo " * 'tidy'      - Run go mod tidy."
	@echo " * 'clean'     - Remove build artifacts."

binaries:
	$(GO_BUILD) -o bin/$(TARGET_BIN) ./cmd/$(TARGET_BIN)

install:
	$(INSTALL) -D -m 755 bin/$(TARGET_BIN) $(BINDIR)/$(TARGET_BIN)

uninstall:
	$(RM) $(BINDIR)/$(TARGET_BIN)

test:
	$(GO) test ./...

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

tidy:
	$(GO) mod tidy

clean:
	$(RM) -r bin _output
