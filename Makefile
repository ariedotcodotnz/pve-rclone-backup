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

.PHONY: all build test race integration perl-test lint fmt vet clean

all: build

build: $(addprefix $(BINDIR)/,$(BINARIES))

$(BINDIR)/%: FORCE
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $@ ./cmd/$*

FORCE:

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
perl-test:
	test/perl/run.sh

fmt:
	gofmt -w $$(git ls-files '*.go')

vet:
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

lint: vet
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; \
	else echo "golangci-lint not installed; skipping (CI runs it)"; fi

clean:
	rm -rf $(BINDIR)
