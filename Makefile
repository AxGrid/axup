.PHONY: all agent axup clean tidy run-help install uninstall

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

AGENT_DIR := internal/agentbin/bin

# Install location. Override e.g. `make install PREFIX=$HOME/.local`.
PREFIX ?= /usr/local
BINDIR := $(PREFIX)/bin

all: axup

# Cross-compile the agent (axupd) for the architectures we ship support for.
agent:
	@mkdir -p $(AGENT_DIR)
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(AGENT_DIR)/axupd-linux-amd64 ./cmd/axupd
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(AGENT_DIR)/axupd-linux-arm64 ./cmd/axupd
	@ls -lh $(AGENT_DIR)

# Build the CLI. Depends on `agent` so the embed.FS has real binaries.
axup: agent
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/axup ./cmd/axup
	@ls -lh bin/axup

# Install the CLI to $(BINDIR). Uses sudo only when needed (writes to
# /usr/local fail without it on most systems). To install without sudo,
# point PREFIX at a directory you own:
#
#   make install PREFIX=$HOME/.local       # → $HOME/.local/bin/axup
#   make install                           # → /usr/local/bin/axup (sudo)
install: axup
	@mkdir -p $(BINDIR) 2>/dev/null || sudo mkdir -p $(BINDIR)
	@if [ -w "$(BINDIR)" ]; then \
		install -m 0755 bin/axup $(BINDIR)/axup; \
	else \
		sudo install -m 0755 bin/axup $(BINDIR)/axup; \
	fi
	@echo "installed $(BINDIR)/axup ($(VERSION))"
	@"$(BINDIR)/axup" version || true

uninstall:
	@if [ -w "$(BINDIR)" ]; then \
		rm -f $(BINDIR)/axup; \
	else \
		sudo rm -f $(BINDIR)/axup; \
	fi
	@echo "removed $(BINDIR)/axup"

tidy:
	go mod tidy

clean:
	rm -rf bin $(AGENT_DIR)/axupd-*

run-help: axup
	./bin/axup --help
