package runner

import (
	"fmt"
	"strings"

	"github.com/axgrid/deploy/internal/protocol"
)

// summary tallies per-status counts across a host's events so the runner can
// print one "summary: changed=N skipped=N …" line per host after the agent's
// done event. Statuses with zero counts are omitted from the printed line to
// keep it short.
type summary struct {
	counts map[string]int
}

func newSummary() *summary { return &summary{counts: map[string]int{}} }

func (s *summary) count(status string) {
	if status == "" {
		return
	}
	s.counts[status]++
}

func (s *summary) empty() bool { return len(s.counts) == 0 }

// line returns the formatted "summary: …" string. The order is fixed so users
// see a stable layout; only non-zero counters are emitted.
func (s *summary) line() string {
	parts := []string{}
	// Order: green-first (good news), then yellow, then gray, then red.
	add := func(label, status string, paint func(string) string) {
		n := s.counts[status]
		if n == 0 {
			return
		}
		parts = append(parts, paint(fmt.Sprintf("%s=%d", label, n)))
	}
	add("changed", protocol.StatusChanged, green)
	add("in_sync", protocol.StatusInSync, green)
	add("would_change", protocol.StatusWouldChange, yellow)
	add("drift", protocol.StatusDrift, yellow)
	add("skipped", protocol.StatusSkipped, gray)
	add("missing", protocol.StatusMissing, red)
	add("error", protocol.StatusError, red)
	if len(parts) == 0 {
		return gray("summary: (no tasks)")
	}
	return gray("summary:") + " " + strings.Join(parts, " ")
}
