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

	"github.com/axgrid/deploy/internal/protocol"
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
	ctx := newRunCtx(state)

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

	if err := state.save(); err != nil {
		w.write(protocol.Event{
			Type:    protocol.EventLog,
			Status:  protocol.StatusError,
			Message: "save state: " + err.Error(),
		})
	}

	w.write(protocol.Event{Type: protocol.EventDone})
	return nil
}

func executeTask(ctx *runCtx, t protocol.Task) protocol.Event {
	// when_changed gates everything except file primitives (which already
	// have their own sha diff). Skipping here keeps each task handler
	// simpler — they assume the gate has already opened.
	if len(t.WhenChanged) > 0 && t.Type != protocol.TaskCopy && t.Type != protocol.TaskTemplate {
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
