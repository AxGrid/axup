.DEFAULT_GOAL := help

.PHONY: help all agent axup install install-latest uninstall release release-archives tidy clean run-help

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILDDATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildDate=$(BUILDDATE)

AGENT_DIR := internal/agentbin/bin
DIST_DIR  := dist

# Platforms shipped as prebuilt release artifacts. The CLI itself runs on
# darwin/linux; the embedded agent always targets linux (it runs on the
# remote), so darwin builds reuse the same internal/agentbin/bin/ payload.
RELEASE_PLATFORMS := darwin-amd64 darwin-arm64 linux-amd64 linux-arm64

# Install location. Override e.g. `make install PREFIX=$HOME/.local`.
PREFIX ?= /usr/local
BINDIR := $(PREFIX)/bin

help: ## Show this help (default when no target is given)
	@awk 'BEGIN { \
	    FS = ":.*?## "; \
	    printf "\nUsage:\n  make \033[36m<target>\033[0m\n"; \
	  } \
	  /^##@ / { printf "\n\033[1m%s\033[0m\n", substr($$0, 5); next } \
	  /^[a-zA-Z0-9_-]+:.*?## / { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	  END { print "" }' $(MAKEFILE_LIST)

##@ Build

all: axup ## Alias for `axup` — cross-build agent + CLI

agent: ## Cross-build axupd (linux/amd64 + linux/arm64) into internal/agentbin/bin
	@mkdir -p $(AGENT_DIR)
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(AGENT_DIR)/axupd-linux-amd64 ./cmd/axupd
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(AGENT_DIR)/axupd-linux-arm64 ./cmd/axupd
	@ls -lh $(AGENT_DIR)

axup: agent ## Build the CLI into ./bin/axup (embeds the agents from `agent`)
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/axup ./cmd/axup
	@ls -lh bin/axup

##@ Install

# Uses sudo only when needed (writes to /usr/local fail without it on most
# systems). To install without sudo, point PREFIX at a directory you own:
#   make install PREFIX=$HOME/.local       # → $HOME/.local/bin/axup
#   make install                           # → /usr/local/bin/axup (sudo)
install: axup ## Install ./bin/axup to $(BINDIR) (override with PREFIX=…)
	@mkdir -p $(BINDIR) 2>/dev/null || sudo mkdir -p $(BINDIR)
	@if [ -w "$(BINDIR)" ]; then \
		install -m 0755 bin/axup $(BINDIR)/axup; \
	else \
		sudo install -m 0755 bin/axup $(BINDIR)/axup; \
	fi
	@echo "installed $(BINDIR)/axup ($(VERSION))"
	@"$(BINDIR)/axup" version || true

install-latest: ## One-liner remote install: `go install` HEAD of master into $(BINDIR)
	@command -v go >/dev/null 2>&1 || { echo "go not found in PATH — install Go from https://go.dev/dl/"; exit 1; }
	@mkdir -p $(BINDIR) 2>/dev/null || sudo mkdir -p $(BINDIR)
	@tmp=$$(mktemp -d); \
		GOBIN=$$tmp GOFLAGS=-trimpath go install github.com/axgrid/axup/cmd/axup@master; \
		if [ -w "$(BINDIR)" ]; then install -m 0755 $$tmp/axup $(BINDIR)/axup; else sudo install -m 0755 $$tmp/axup $(BINDIR)/axup; fi; \
		rm -rf $$tmp
	@"$(BINDIR)/axup" version || true

uninstall: ## Remove $(BINDIR)/axup
	@if [ -w "$(BINDIR)" ]; then \
		rm -f $(BINDIR)/axup; \
	else \
		sudo rm -f $(BINDIR)/axup; \
	fi
	@echo "removed $(BINDIR)/axup"

##@ Release

release: agent ## Cross-build axup for darwin+linux / amd64+arm64 into ./dist/
	@rm -rf $(DIST_DIR) && mkdir -p $(DIST_DIR)
	@set -e; for plat in $(RELEASE_PLATFORMS); do \
		os=$${plat%-*}; arch=$${plat#*-}; \
		echo "  -> $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST_DIR)/axup-$$os-$$arch ./cmd/axup; \
	done
	@ls -lh $(DIST_DIR)

release-archives: release ## Wrap each release binary in a .tar.gz + write SHA256SUMS
	@cd $(DIST_DIR) && rm -f SHA256SUMS && for b in axup-*; do \
		case $$b in *.tar.gz|SHA256SUMS) continue ;; esac; \
		tar czf $$b.tar.gz $$b; \
		shasum -a 256 $$b.tar.gz >> SHA256SUMS; \
	done
	@ls -lh $(DIST_DIR)

##@ Housekeeping

tidy: ## Run `go mod tidy`
	go mod tidy

clean: ## Remove ./bin, ./dist, and the cross-compiled agent binaries
	rm -rf bin $(DIST_DIR) $(AGENT_DIR)/axupd-*

##@ Inspect

run-help: axup ## Build the CLI and print `axup --help`
	./bin/axup --help
