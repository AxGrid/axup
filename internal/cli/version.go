package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(versionString())
	},
}

func versionString() string {
	parts := []string{"axup " + Version}
	if Commit != "" && Commit != "none" {
		parts = append(parts, "commit="+Commit)
	}
	if BuildDate != "" && BuildDate != "unknown" {
		parts = append(parts, "built="+BuildDate)
	}
	parts = append(parts, fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH))
	return joinSpace(parts)
}

func joinSpace(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}
