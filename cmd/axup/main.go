// Command axup is the local CLI. It parses rulebooks, uploads the embedded
// agent over SSH, and orchestrates bootstrap/deploy runs against target hosts.
package main

import (
	"fmt"
	"os"

	"github.com/axgrid/axup/internal/cli"
)

// Version is overridden at build time via -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	cli.Version = Version
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
