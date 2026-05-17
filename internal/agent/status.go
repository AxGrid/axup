package agent

import (
	"fmt"
	"os"
	"sort"

	"github.com/axgrid/axup/internal/protocol"
)

// emitStatus walks the loaded state and emits one synthetic task_end event
// per tracked file with one of in_sync / drift / missing. A header EventLog
// carries the rulebook name and the last-applied timestamp so the CLI can
// render a section title before the per-file rows.
//
// When includeHistory is true (set by Plan.IncludeHistory), each per-file
// event also carries the per-file history chain in ev.History so the CLI
// can render a "history:" sub-block for `axup status --history`.
func emitStatus(w *eventWriter, state *State, includeHistory bool) {
	w.write(protocol.Event{
		Type:    protocol.EventLog,
		Message: fmt.Sprintf("rulebook=%s updated_at=%s files=%d", state.RulebookName, state.UpdatedAt, len(state.Files)),
	})

	// Stable order so multi-host outputs are easier to compare visually.
	paths := make([]string, 0, len(state.Files))
	for p := range state.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for i, p := range paths {
		fs := state.Files[p]
		taskID := fmt.Sprintf("status.%d", i+1)
		w.write(protocol.Event{Type: protocol.EventTaskStart, TaskID: taskID, Message: p})

		ev := protocol.Event{Type: protocol.EventTaskEnd, TaskID: taskID, Path: p}
		curSha, curMode, exists, err := inspect(p)
		switch {
		case err != nil:
			ev.Status = protocol.StatusError
			ev.Message = "stat: " + err.Error()
		case !exists:
			ev.Status = protocol.StatusMissing
			ev.Message = "expected at " + p + " (sha " + short(fs.Sha256) + ", mode " + fs.Mode + ") but file is gone"
		default:
			modeOK := fs.Mode == "" || fmtMode(curMode) == fs.Mode
			shaOK := curSha == fs.Sha256
			if shaOK && modeOK {
				ev.Status = protocol.StatusInSync
				ev.Message = fmt.Sprintf("sha %s mode %s applied_at %s", short(fs.Sha256), fs.Mode, fs.AppliedAt)
			} else {
				ev.Status = protocol.StatusDrift
				switch {
				case !shaOK && !modeOK:
					ev.Message = fmt.Sprintf("sha differs (%s → %s) AND mode differs (%s → %s)", short(fs.Sha256), short(curSha), fs.Mode, fmtMode(curMode))
				case !shaOK:
					ev.Message = fmt.Sprintf("sha differs: state=%s disk=%s", short(fs.Sha256), short(curSha))
				default:
					ev.Message = fmt.Sprintf("mode differs: state=%s disk=%s", fs.Mode, fmtMode(curMode))
				}
			}
		}
		if includeHistory && len(fs.History) > 0 {
			ev.History = make([]protocol.HistoryItem, 0, len(fs.History))
			for _, h := range fs.History {
				ev.History = append(ev.History, protocol.HistoryItem{
					Sha256:     h.Sha256,
					Mode:       h.Mode,
					RecordedAt: h.RecordedAt,
					Phase:      h.Phase,
				})
			}
		}
		w.write(ev)
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func fmtMode(m os.FileMode) string {
	// Match the "0644" form we store in state.json.
	return fmt.Sprintf("%04o", m.Perm())
}
