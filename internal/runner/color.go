package runner

import (
	"os"

	"golang.org/x/term"
)

// Color output is enabled when stdout is a TTY AND neither NO_COLOR (de-facto
// standard env var) nor --no-color is set. NoColor is toggled at startup from
// the CLI flag, see cli.applyColorFlag.
var (
	colorEnabled bool
	NoColor      bool // set by the CLI before any Run call
)

func init() {
	colorEnabled = term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == ""
}

// recompute is called once per Run to honour the late-binding NoColor flag.
func recomputeColor() {
	if NoColor {
		colorEnabled = false
		return
	}
	colorEnabled = term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == ""
}

func paint(code, s string) string {
	if !colorEnabled {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func green(s string) string  { return paint("32", s) }
func red(s string) string    { return paint("31", s) }
func yellow(s string) string { return paint("33", s) }
func cyan(s string) string   { return paint("36", s) }
func gray(s string) string   { return paint("90", s) }
func bold(s string) string   { return paint("1", s) }
