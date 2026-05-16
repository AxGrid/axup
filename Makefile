.PHONY: all agent deploy clean tidy run-help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

AGENT_DIR := internal/agentbin/bin

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

tidy:
	go mod tidy

clean:
	rm -rf bin $(AGENT_DIR)/deployd-*

run-help: deploy
	./bin/deploy --help
