# Local development targets. Releases are built by GoReleaser from tags.
#
# Local builds carry no release version: swarm reads the commit (and a +dirty
# marker for uncommitted changes) from Go's build metadata. `swarm repo init`
# relies on that to pin generated workflows to a clean build's exact commit,
# so do not stamp a version here.

GO        ?= go
GOBIN     ?= $(shell "$(GO)" env GOPATH)/bin
BIN_DIR   ?= bin

.PHONY: all build install test race fmt lint check clean version

all: check build

## build: compile swarm and the Linux restore helper from this checkout
build:
	"$(GO)" build -o "$(BIN_DIR)/swarm" ./cmd/swarm
	@if [ "$$("$(GO)" env GOOS)" = linux ]; then CGO_ENABLED=0 "$(GO)" build -o "$(BIN_DIR)/swarm-restore-helper" ./cmd/swarm-restore-helper; fi

## install: install swarm and the Linux restore helper into $(GOBIN) (default $$HOME/go/bin) from this checkout
install:
	GOBIN="$(GOBIN)" "$(GO)" install ./cmd/swarm
	@if [ "$$("$(GO)" env GOOS)" = linux ]; then CGO_ENABLED=0 GOBIN="$(GOBIN)" "$(GO)" install ./cmd/swarm-restore-helper; fi
	@"$(GOBIN)/swarm" version

## test: run the test suite
test:
	$(GO) test ./...

## race: run the test suite with the race detector, as CI does
race:
	$(GO) test -race ./...

## fmt: format Go sources
fmt:
	gofmt -s -w cmd internal

## lint: check formatting, vet, and staticcheck
lint:
	@unformatted=$$(gofmt -s -l cmd internal); if [ -n "$$unformatted" ]; then echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...
	$(GO) tool staticcheck ./...

## check: everything CI checks before a PR (lint, race tests, and changelog fragments)
check: lint race changelog-check

## version: show the installed swarm version
version:
	@$(GOBIN)/swarm version

## clean: remove local build output
clean:
	rm -rf $(BIN_DIR)

.PHONY: changelog changelog-check

## changelog: fold unreleased fragments into VERSION (maintainers)
changelog:
	$(GO) run ./cmd/changelog "$(VERSION)"

## changelog-check: validate unreleased fragments without writing
changelog-check:
	$(GO) run ./cmd/changelog --check
