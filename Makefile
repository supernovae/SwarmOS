# Local development targets. Releases are built by GoReleaser from tags.
#
# Local builds carry no release version: swarm reads the commit (and a +dirty
# marker for uncommitted changes) from Go's build metadata. `swarm repo init`
# relies on that to pin generated workflows to a clean build's exact commit,
# so do not stamp a version here.

GO        ?= go
GOBIN     ?= $(shell $(GO) env GOPATH)/bin
BIN_DIR   ?= bin

.PHONY: all build install test race fmt lint check clean version

all: check build

## build: compile ./bin/swarm from this checkout
build:
	$(GO) build -o $(BIN_DIR)/swarm ./cmd/swarm

## install: install swarm into $(GOBIN) (default $$HOME/go/bin) from this checkout
install:
	GOBIN=$(GOBIN) $(GO) install ./cmd/swarm
	@$(GOBIN)/swarm version

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

## check: everything CI checks before a PR (lint and race tests)
check: lint race

## version: show the installed swarm version
version:
	@$(GOBIN)/swarm version

## clean: remove local build output
clean:
	rm -rf $(BIN_DIR)

.PHONY: changelog

## changelog: fold unreleased fragments into VERSION (maintainers)
changelog:
	$(GO) run ./cmd/changelog "$(VERSION)"
