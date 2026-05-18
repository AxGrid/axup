package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(versionString())
	},
}

// versionString assembles a "axup <version> commit=<sha> built=<date> <os>/<arch>"
// line. When linker -X flags were set by the Makefile the values come straight
// from the package-level vars; for `go install`-style builds (no ldflags) we
// fall back to runtime/debug.BuildInfo so the binary still self-reports a
// useful module version + vcs revision.
func versionString() string {
	v, c, d := Version, Commit, BuildDate
	if v == "dev" || c == "none" || d == "unknown" {
		bv, bc, bd := buildInfoVersion()
		if v == "dev" && bv != "" {
			v = bv
		}
		if c == "none" && bc != "" {
			c = bc
		}
		if d == "unknown" && bd != "" {
			d = bd
		}
	}

	parts := []string{"axup " + v}
	if c != "" && c != "none" {
		parts = append(parts, "commit="+c)
	}
	if d != "" && d != "unknown" {
		parts = append(parts, "built="+d)
	}
	parts = append(parts, fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH))
	return strings.Join(parts, " ")
}

// buildInfoVersion pulls module + VCS info from the binary's embedded
// debug.BuildInfo. Populated automatically by `go build`/`go install` when
// -buildvcs is on (the default since Go 1.18) — gives us a working
// self-version for binaries produced outside the Makefile pipeline.
func buildInfoVersion() (version, commit, date string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	var revision, modtime string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			modtime = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision != "" {
		commit = revision
		if len(commit) > 7 {
			commit = commit[:7]
		}
		if modified {
			commit += "-dirty"
		}
	}
	date = modtime
	return
}
