package agent

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/axgrid/axup/internal/protocol"
)

// runRollback restores tracked files from their archived history. The
// agent walks state.Files, and for each file:
//
//  1. If RollbackTask filter is set and doesn't match this path → skip
//     (emit task_end with status=skipped so the CLI's per-host summary
//     still shows what was considered).
//  2. If the file has fewer than `step` history entries → emit error.
//  3. Otherwise: copy History[step-1].ArchivedPath back onto the live path
//     (chmod to History[step-1].Mode), update FileState.{Sha256,Mode} to
//     the restored values, drop History[0..step] (the rolled-over entries
//     including the one we just promoted to "current"), best-effort rm
//     their archive files. New history = old History[step:].
//
// Dry-run is gated by ctx.dryRun in the caller — this function only runs
// when state mutations are intended.
//
// Reset semantics (vs. rotate): the rolled-over entries are *gone* after
// rollback, including the version that was current before. Rationale: ops
// "go back to version N" usually means version N+1 is obsolete and
// shouldn't be reachable as a future rollback target. Rotate-style "swap
// current with N" would surprise on a second rollback ("why did I get
// the new version back?"). Documented in CLAUDE.md and the user-facing
// `axup rollback --help`.
func runRollback(w *eventWriter, state *State, step int, taskFilter string) bool {
	if step < 1 {
		w.write(protocol.Event{
			Type:    protocol.EventLog,
			Status:  protocol.StatusError,
			Message: fmt.Sprintf("rollback step must be >= 1 (got %d)", step),
		})
		return true
	}
	w.write(protocol.Event{
		Type:    protocol.EventLog,
		Message: fmt.Sprintf("rulebook=%s rollback_step=%d filter=%q tracked_files=%d", state.RulebookName, step, taskFilter, len(state.Files)),
	})

	// Stable order so multi-host outputs are comparable visually.
	paths := make([]string, 0, len(state.Files))
	for p := range state.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	anyAttempted := false
	hadError := false
	for i, path := range paths {
		fs := state.Files[path]
		taskID := fmt.Sprintf("rollback.%d", i+1)
		if taskFilter != "" && taskFilter != path {
			continue
		}
		anyAttempted = true
		// Files with no history at all are silently skipped UNLESS the
		// user explicitly targeted them with --task. Rationale: rolling
		// back the whole rulebook shouldn't fail just because some files
		// were only written once (no history to roll back to is the
		// expected state for those, not an error).
		explicit := taskFilter != ""
		if len(fs.History) == 0 && !explicit {
			w.write(protocol.Event{Type: protocol.EventTaskStart, TaskID: taskID, Message: path})
			w.write(protocol.Event{
				Type: protocol.EventTaskEnd, TaskID: taskID, Path: path,
				Status:  protocol.StatusSkipped,
				Message: "no history captured for this file",
			})
			continue
		}
		w.write(protocol.Event{Type: protocol.EventTaskStart, TaskID: taskID, Message: path})
		ev := rollbackOne(fs, path, step)
		ev.Type = protocol.EventTaskEnd
		ev.TaskID = taskID
		if ev.Status == protocol.StatusError {
			hadError = true
		}
		w.write(ev)
	}
	if !anyAttempted {
		w.write(protocol.Event{
			Type:    protocol.EventLog,
			Status:  protocol.StatusError,
			Message: fmt.Sprintf("no tracked file matched filter %q", taskFilter),
		})
		hadError = true
	}
	return hadError
}

// rollbackOne restores a single file. Errors are reported via the returned
// Event; state mutations happen in place.
func rollbackOne(fs *FileState, path string, step int) protocol.Event {
	if len(fs.History) < step {
		return protocol.Event{
			Status:  protocol.StatusError,
			Path:    path,
			Message: fmt.Sprintf("not enough history (have %d, requested step %d)", len(fs.History), step),
		}
	}
	target := fs.History[step-1]

	mode, err := parseMode(target.Mode)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: path, Message: "bad archived mode " + target.Mode + ": " + err.Error()}
	}

	// Read archive → write atomically to live path.
	src, err := os.Open(target.ArchivedPath)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: path, Message: "open archive: " + err.Error()}
	}
	tmp := path + ".rollback.tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		_ = src.Close()
		return protocol.Event{Status: protocol.StatusError, Path: path, Message: "create tmp: " + err.Error()}
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = src.Close()
		_ = dst.Close()
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: path, Message: "copy archive: " + err.Error()}
	}
	_ = src.Close()
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: path, Message: "close tmp: " + err.Error()}
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: path, Message: "chmod tmp: " + err.Error()}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return protocol.Event{Status: protocol.StatusError, Path: path, Message: "rename tmp: " + err.Error()}
	}

	// Reset-semantics: drop entries [0..step) — they're gone, including
	// the version that was "current" before. rm best-effort.
	for _, dropped := range fs.History[:step] {
		_ = os.Remove(dropped.ArchivedPath)
	}
	newHistory := append([]HistoryEntry(nil), fs.History[step:]...)

	fs.Sha256 = target.Sha256
	fs.Mode = target.Mode
	fs.AppliedAt = time.Now().UTC().Format(time.RFC3339)
	fs.History = newHistory

	return protocol.Event{
		Status:  protocol.StatusChanged,
		Path:    path,
		Message: fmt.Sprintf("restored sha=%s mode=%s recorded_at=%s phase=%s (%d entries dropped, %d remaining)", short(target.Sha256), target.Mode, target.RecordedAt, target.Phase, step, len(newHistory)),
	}
}

func parseMode(modeStr string) (os.FileMode, error) {
	if modeStr == "" {
		return 0o644, nil
	}
	v, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return 0, err
	}
	return os.FileMode(v), nil
}

// emitRollbackPreview is the dry-run counterpart of runRollback. Reports
// what WOULD be restored without touching state or files.
func emitRollbackPreview(w *eventWriter, state *State, step int, taskFilter string) {
	if step < 1 {
		w.write(protocol.Event{
			Type:    protocol.EventLog,
			Status:  protocol.StatusError,
			Message: fmt.Sprintf("rollback step must be >= 1 (got %d)", step),
		})
		return
	}
	w.write(protocol.Event{
		Type:    protocol.EventLog,
		Message: fmt.Sprintf("rulebook=%s rollback_step=%d filter=%q tracked_files=%d (dry-run)", state.RulebookName, step, taskFilter, len(state.Files)),
	})
	paths := make([]string, 0, len(state.Files))
	for p := range state.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for i, path := range paths {
		fs := state.Files[path]
		if taskFilter != "" && taskFilter != path {
			continue
		}
		taskID := fmt.Sprintf("rollback.%d", i+1)
		explicit := taskFilter != ""
		if len(fs.History) == 0 && !explicit {
			w.write(protocol.Event{Type: protocol.EventTaskStart, TaskID: taskID, Message: path})
			w.write(protocol.Event{
				Type: protocol.EventTaskEnd, TaskID: taskID, Path: path,
				Status:  protocol.StatusSkipped,
				Message: "no history captured for this file",
			})
			continue
		}
		w.write(protocol.Event{Type: protocol.EventTaskStart, TaskID: taskID, Message: path})
		ev := protocol.Event{Type: protocol.EventTaskEnd, TaskID: taskID, Path: path}
		if len(fs.History) < step {
			ev.Status = protocol.StatusError
			ev.Message = fmt.Sprintf("not enough history (have %d, requested step %d)", len(fs.History), step)
		} else {
			t := fs.History[step-1]
			ev.Status = protocol.StatusWouldChange
			ev.Message = fmt.Sprintf("would restore sha=%s mode=%s recorded_at=%s phase=%s (would drop %d entries, %d would remain)", short(t.Sha256), t.Mode, t.RecordedAt, t.Phase, step, len(fs.History)-step)
		}
		w.write(ev)
	}
}
