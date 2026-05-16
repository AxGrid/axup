// Command axupd is the remote agent. It is uploaded to the target server by
// the axup CLI, reads a Plan from stdin, and emits Events on stdout.
package main

import (
	"fmt"
	"os"

	"github.com/axgrid/axup/internal/agent"
)

var Version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(Version)
		return
	}
	if err := agent.Run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "axupd:", err)
		os.Exit(1)
	}
}
