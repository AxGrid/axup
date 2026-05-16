.PHONY: all agent deploy clean tidy run-help install uninstall

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

AGENT_DIR := internal/agentbin/bin

# Install location. Override e.g. `make install PREFIX=$HOME/.local`.
PREFIX ?= /usr/local
BINDIR := $(PREFIX)/bin

all: deploy

# Cross-compile the agent for the architectures we ship support for.
agent:
	@mkdir -p $(AGENT_DIR)
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(AGENT_DIR)/deployd-linux-amd64 ./cmd/deployd
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(AGENT_DIR)/deployd-linux-arm64 ./cmd/deployd
	@ls -lh $(AGENT_DIR)

# Build the CLI. Depends on `agent` so the embed.FS has real binaries.
deploy: agent
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/deploy ./cmd/deploy
	@ls -lh bin/deploy

# Install the CLI to $(BINDIR). Uses sudo only when needed (writes to
# /usr/local fail without it on most systems). To install without sudo,
# point PREFIX at a directory you own:
#
#   make install PREFIX=$HOME/.local       # → $HOME/.local/bin/deploy
#   make install                           # → /usr/local/bin/deploy (sudo)
install: deploy
	@mkdir -p $(BINDIR) 2>/dev/null || sudo mkdir -p $(BINDIR)
	@if [ -w "$(BINDIR)" ]; then \
		install -m 0755 bin/deploy $(BINDIR)/deploy; \
	else \
		sudo install -m 0755 bin/deploy $(BINDIR)/deploy; \
	fi
	@echo "installed $(BINDIR)/deploy ($(VERSION))"
	@"$(BINDIR)/deploy" version || true

uninstall:
	@if [ -w "$(BINDIR)" ]; then \
		rm -f $(BINDIR)/deploy; \
	else \
		sudo rm -f $(BINDIR)/deploy; \
	fi
	@echo "removed $(BINDIR)/deploy"

tidy:
	go mod tidy

clean:
	rm -rf bin $(AGENT_DIR)/deployd-*

run-help: deploy
	./bin/deploy --help
