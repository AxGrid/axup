// Package agent is the remote runtime. It runs on the target server, reads a
// Plan from stdin, executes tasks, persists state, and emits Events on stdout.
// Anything the agent prints to stderr is treated by the CLI as crash/diagnostic
// noise — all structured output goes through stdout.
package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/axgrid/axup/internal/protocol"
)

func Run(in io.Reader, out io.Writer) error {
	w := &eventWriter{out: out}

	var plan protocol.Plan
	dec := json.NewDecoder(in)
	if err := dec.Decode(&plan); err != nil {
		return fmt.Errorf("decode plan: %w", err)
	}
	if plan.RulebookName == "" {
		return fmt.Errorf("plan is missing rulebook_name")
	}

	state, err := loadState(plan.RulebookName)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	ctx := newRunCtx(state, plan.DryRun, plan.Diff, plan.KeepHistory, plan.Phase)

	// Status-only requests bypass task execution entirely. The agent reads
	// state.json, walks every recorded file, and emits a synthetic task_end
	// per file describing in_sync / drift / missing. No state.save() either —
	// status is read-only.
	if plan.StatusOnly {
		emitStatus(w, state, plan.IncludeHistory)
		w.write(protocol.Event{Type: protocol.EventDone})
		return nil
	}

	// Clear-history mode: wipe every FileState.History + remove the
	// history dir. Dry-run reports what would be cleared without
	// touching state. No other tasks run.
	if plan.ClearHistory {
		total := 0
		for _, fs := range state.Files {
			total += len(fs.History)
		}
		if plan.DryRun {
			w.write(protocol.Event{
				Type:    protocol.EventLog,
				Status:  protocol.StatusWouldChange,
				Message: fmt.Sprintf("would clear %d history entries across %d tracked files", total, len(state.Files)),
			})
		} else {
			dropped, err := state.clearAllHistory()
			if err != nil {
				w.write(protocol.Event{
					Type:    protocol.EventLog,
					Status:  protocol.StatusError,
					Message: "clear history: " + err.Error(),
				})
			} else {
				w.write(protocol.Event{
					Type:    protocol.EventLog,
					Status:  protocol.StatusChanged,
					Message: fmt.Sprintf("cleared %d history entries across %d tracked files", dropped, len(state.Files)),
				})
			}
			if err := state.save(); err != nil {
				w.write(protocol.Event{
					Type:    protocol.EventLog,
					Status:  protocol.StatusError,
					Message: "save state: " + err.Error(),
				})
			}
		}
		w.write(protocol.Event{Type: protocol.EventDone})
		return nil
	}

	// Rollback mode: ignore Tasks entirely, walk state.json, restore each
	// tracked file from its history chain. Dry-run is respected — preview
	// without touching anything (handled inline in the rollback path).
	if plan.Rollback {
		hadError := false
		if plan.DryRun {
			emitRollbackPreview(w, state, plan.RollbackStep, plan.RollbackTask)
		} else {
			hadError = runRollback(w, state, plan.RollbackStep, plan.RollbackTask)
			if err := state.save(); err != nil {
				w.write(protocol.Event{
					Type:    protocol.EventLog,
					Status:  protocol.StatusError,
					Message: "save state: " + err.Error(),
				})
				hadError = true
			}
		}
		_ = hadError // surfaces via per-task status=error events; CLI counts them
		w.write(protocol.Event{Type: protocol.EventDone})
		return nil
	}

	for _, task := range plan.Tasks {
		w.write(protocol.Event{
			Type:    protocol.EventTaskStart,
			TaskID:  task.ID,
			Message: task.Name,
		})
		ev := executeTask(ctx, task)
		ev.Type = protocol.EventTaskEnd
		ev.TaskID = task.ID
		w.write(ev)
	}

	// In dry-run we keep state on disk exactly as it was — nothing actually
	// got applied this run.
	if !ctx.dryRun {
		if err := state.save(); err != nil {
			w.write(protocol.Event{
				Type:    protocol.EventLog,
				Status:  protocol.StatusError,
				Message: "save state: " + err.Error(),
			})
		}
	}

	w.write(protocol.Event{Type: protocol.EventDone})
	return nil
}

func executeTask(ctx *runCtx, t protocol.Task) protocol.Event {
	// when_changed gates everything except file primitives (which already
	// have their own sha/stat-based diff). Skipping here keeps each task
	// handler simpler — they assume the gate has already opened. Parser
	// already rejects when_changed on these types, but we re-check at
	// runtime in case a forged plan slips through.
	if len(t.WhenChanged) > 0 &&
		t.Type != protocol.TaskCopy &&
		t.Type != protocol.TaskTemplate &&
		t.Type != protocol.TaskMkdir &&
		t.Type != protocol.TaskSymlink &&
		t.Type != protocol.TaskRemove {
		any := false
		for _, p := range t.WhenChanged {
			if ctx.changed[p] {
				any = true
				break
			}
		}
		if !any {
			return protocol.Event{
				Status:  protocol.StatusSkipped,
				Message: "no watched paths changed",
			}
		}
	}

	switch t.Type {
	case protocol.TaskCommand:
		return runCommandTask(ctx, t)
	case protocol.TaskCopy, protocol.TaskTemplate:
		return runFileTask(ctx, t)
	case protocol.TaskMkdir:
		return runMkdirTask(ctx, t)
	case protocol.TaskSymlink:
		return runSymlinkTask(ctx, t)
	case protocol.TaskRemove:
		return runRemoveTask(ctx, t)
	case protocol.TaskUser:
		return runUserTask(ctx, t)
	case protocol.TaskGroup:
		return runGroupTask(ctx, t)
	case protocol.TaskDownload:
		return runDownloadTask(ctx, t)
	case protocol.TaskApt:
		return runAptTask(ctx, t)
	case protocol.TaskService:
		return runServiceTask(ctx, t)
	case protocol.TaskDockerCompose:
		return runDockerComposeTask(ctx, t)
	case protocol.TaskDockerInstall:
		return runDockerInstallTask(ctx, t)
	case protocol.TaskDockerLogin:
		return runDockerLoginTask(ctx, t)
	default:
		return protocol.Event{
			Status:  protocol.StatusError,
			Message: fmt.Sprintf("unknown task type: %q", t.Type),
		}
	}
}

// eventWriter serializes Event writes so the agent can be safely extended to
// emit events from multiple goroutines later (parallel tasks, streaming).
type eventWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *eventWriter) write(ev protocol.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, _ := json.Marshal(ev)
	b = append(b, '\n')
	_, _ = w.out.Write(b)
}
