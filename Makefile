# Vigil Agent — developer Makefile.
#
# `make help` lists targets. The release pipeline (goreleaser) builds
# artefacts straight from .goreleaser.yaml in CI; `make release-snapshot`
# here is a thin wrapper for local dry-runs. Everything else is for local
# dev and CI.

SHELL := /usr/bin/env bash

# Build metadata injected into internal/version at link time. Falls back to
# sane defaults so `make build` works on a clean checkout with no git history.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG          := github.com/Seppia-AI/vigil-agent
LDFLAGS      := -s -w \
                -X $(PKG)/internal/version.Version=$(VERSION) \
                -X $(PKG)/internal/version.Commit=$(COMMIT) \
                -X $(PKG)/internal/version.Date=$(DATE)
GO_BUILD     := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

BIN_DIR := bin
BIN     := $(BIN_DIR)/vigil-agent

# Cross-build matrix. Windows is not currently included.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.DEFAULT_GOAL := help

.PHONY: help
help: ## show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## build the agent for the host platform into ./bin
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN) ./cmd/vigil-agent
	@echo "built $(BIN) ($(VERSION))"

.PHONY: cross
cross: ## cross-build the agent for every supported platform into ./bin
	@mkdir -p $(BIN_DIR)
	@set -e; for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=$(BIN_DIR)/vigil-agent-$$os-$$arch; \
		echo "→ $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO_BUILD) -o $$out ./cmd/vigil-agent; \
	done

.PHONY: test
test: ## run unit tests with race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## run tests with coverage; writes coverage.txt + coverage.html
	go test -race -count=1 -coverprofile=coverage.txt -covermode=atomic ./...
	go tool cover -html=coverage.txt -o coverage.html
	@echo "open coverage.html"

.PHONY: lint
lint: ## run golangci-lint (install: https://golangci-lint.run/usage/install/)
	golangci-lint run ./...

.PHONY: fmt
fmt: ## format code with gofmt
	gofmt -s -w .

.PHONY: tidy
tidy: ## tidy go.mod / go.sum
	go mod tidy

.PHONY: run
run: ## run the agent locally (no build artefact)
	go run ./cmd/vigil-agent $(ARGS)

.PHONY: clean
clean: ## remove build artefacts
	rm -rf $(BIN_DIR) dist coverage.txt coverage.html

# ─── Release ─────────────────────────────────────────────────────────────────
#
# These targets shell out to goreleaser. Install:
#   brew install goreleaser   # macOS
#   # or follow https://goreleaser.com/install/
#
# `release-snapshot` is the safe local dry-run: builds every artefact into
# ./dist/ but does NOT publish or sign anything. Use it to sanity-check
# .goreleaser.yaml before pushing a tag.

GORELEASER ?= goreleaser

.PHONY: release-check
release-check: ## lint .goreleaser.yaml without building anything
	$(GORELEASER) check

.PHONY: release-snapshot
release-snapshot: ## build a local snapshot release into ./dist (no upload, no sign)
	$(GORELEASER) release --snapshot --clean --skip=publish,sign

.PHONY: release
release: ## (CI ONLY) build + publish a real release. Requires a pushed git tag.
	@if [ -z "$$GITHUB_TOKEN" ]; then \
		echo "GITHUB_TOKEN is not set; refusing to publish."; exit 1; \
	fi
	$(GORELEASER) release --clean
