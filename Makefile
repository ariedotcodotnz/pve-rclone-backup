# SPDX-License-Identifier: AGPL-3.0-or-later

GO        ?= go
PKG       := github.com/ariedotcodotnz/pve-rclone-backup
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS   := -s -w -X github.com/rclone/rclone/fs.VersionSuffix= -X $(PKG)/internal/version.Version=$(VERSION) -X $(PKG)/internal/version.Commit=$(COMMIT)
GOFLAGS   ?= -trimpath
BINDIR    ?= bin
export CGO_ENABLED ?= 0

BINARIES := pve-rclone-backupd pve-rclone-backup

.PHONY: all build generate test race integration perl-test deb deb-test lint fmt vet clean

all: build

build: $(addprefix $(BINDIR)/,$(BINARIES))

$(BINDIR)/%: FORCE
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/$*

FORCE:

# Regenerate configuration code from schema/*.yaml.
generate:
	$(GO) run ./cmd/schemagen -root .

test:
	$(GO) test ./...

# The race detector needs cgo.
race:
	CGO_ENABLED=1 $(GO) test -race ./...

# Integration tests exercise the embedded rclone engine against local
# backends; they need no network access.
integration:
	$(GO) test -tags integration -count=1 ./...

# Perl plugin tests run against Proxmox VE's real storage library in a
# container (needs docker).
perl-test: $(BINDIR)/pve-rclone-backupd $(BINDIR)/pve-rclone-backup
	test/perl/run.sh

# Debian package version: 0.1.0~dev.<date>.g<commit> until releases are tagged.
DEB_VERSION ?= 0.1.0~dev.$(shell date -u +%Y%m%d).g$(shell git rev-parse --short HEAD 2>/dev/null || echo 0)
DEB_ARCH    ?= amd64

# Static binaries, so the package depends on nothing but PVE and the tools it runs.
deb:
	CGO_ENABLED=0 GOARCH=$(DEB_ARCH) $(MAKE) build VERSION=$(DEB_VERSION)
	packaging/build-deb.sh $(DEB_VERSION) $(DEB_ARCH) $(BINDIR) dist

# Install, use and purge the package in the PVE test container.
deb-test: deb
	test/deb/run.sh dist/pve-rclone-backup_$(DEB_VERSION)_$(DEB_ARCH).deb

fmt:
	gofmt -w $$(git ls-files '*.go')

vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

lint: vet
	$(GO) run ./cmd/schemagen -root . -check
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else echo "golangci-lint not installed; skipping (CI runs it)"; fi

clean:
	rm -rf $(BINDIR)
