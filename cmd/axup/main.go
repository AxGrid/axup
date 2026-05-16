// Command axup is the local CLI. It parses rulebooks, uploads the embedded
// agent over SSH, and orchestrates bootstrap/deploy runs against target hosts.
package main

import (
	"fmt"
	"os"

	"github.com/axgrid/axup/internal/cli"
)

// Version, Commit and BuildDate are overridden at build time via
// -ldflags "-X main.Version=... -X main.Commit=... -X main.BuildDate=...".
// The Makefile populates them from `git describe --tags --always --dirty`,
// `git rev-parse --short HEAD` and `date -u +%Y-%m-%dT%H:%M:%SZ`.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func main() {
	cli.Version = Version
	cli.Commit = Commit
	cli.BuildDate = BuildDate
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
