// Package agentbin embeds the cross-compiled deployd binaries that ship inside
// the deploy CLI. The Makefile builds them into ./bin/ before compiling the
// CLI. Placeholder files exist so `go build` works without `make`, but the
// resulting CLI will refuse to deploy until real binaries are present.
package agentbin

import (
	_ "embed"
	"fmt"
)

//go:embed bin/deployd-linux-amd64
var linuxAmd64 []byte

//go:embed bin/deployd-linux-arm64
var linuxArm64 []byte

// Binary returns the agent payload for the given GOARCH. Returns an error if
// the embedded binary is empty (placeholder), which means the user built the
// CLI without running `make agent` first.
func Binary(goarch string) ([]byte, error) {
	var b []byte
	switch goarch {
	case "amd64":
		b = linuxAmd64
	case "arm64":
		b = linuxArm64
	default:
		return nil, fmt.Errorf("no embedded agent for GOARCH=%s", goarch)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("embedded agent for %s is empty — run `make agent` and rebuild the CLI", goarch)
	}
	return b, nil
}
