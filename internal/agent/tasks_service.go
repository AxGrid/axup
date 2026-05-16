package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/axgrid/axup/internal/protocol"
)

func runServiceTask(ctx *runCtx, t protocol.Task) protocol.Event {
	provider := t.ServiceProvider
	if provider == "" {
		provider = "systemd"
	}
	state := t.ServiceState
	if state == "" {
		state = "started"
	}
	switch provider {
	case "systemd":
		return runSystemdService(ctx, t.ServiceName, state, t.ServiceEnabled)
	case "supervisor":
		return runSupervisorService(ctx, t.ServiceName, state)
	default:
		return protocol.Event{Status: protocol.StatusError, Message: "unknown service provider: " + provider}
	}
}

// sysOp is one decision the service handler made for the current state. A
// non-empty verb means "needs systemctl <verb>"; empty verb is a no-op note
// describing why nothing was done.
type sysOp struct {
	verb string
	done string // past-tense note used both for real apply and dry-run preview
}

func decideSystemd(active, enabledNow, state string, enabled *bool) []sysOp {
	var ops []sysOp
	switch state {
	case "started":
		if active != "active" {
			ops = append(ops, sysOp{verb: "start", done: "started"})
		} else {
			ops = append(ops, sysOp{done: "already active"})
		}
	case "stopped":
		if active == "active" {
			ops = append(ops, sysOp{verb: "stop", done: "stopped"})
		} else {
			ops = append(ops, sysOp{done: "already inactive"})
		}
	case "restarted":
		ops = append(ops, sysOp{verb: "restart", done: "restarted"})
	case "reloaded":
		ops = append(ops, sysOp{verb: "reload", done: "reloaded"})
	}
	if enabled != nil {
		switch {
		case *enabled && enabledNow != "enabled":
			ops = append(ops, sysOp{verb: "enable", done: "enabled"})
		case !*enabled && enabledNow == "enabled":
			ops = append(ops, sysOp{verb: "disable", done: "disabled"})
		}
	}
	return ops
}

func runSystemdService(ctx *runCtx, name, state string, enabled *bool) protocol.Event {
	active, _ := systemctlIs("is-active", name)
	enabledNow, _ := systemctlIs("is-enabled", name)
	ops := decideSystemd(active, enabledNow, state, enabled)

	needsChange := false
	for _, op := range ops {
		if op.verb != "" {
			needsChange = true
			break
		}
	}
	if !needsChange {
		notes := make([]string, 0, len(ops))
		for _, op := range ops {
			notes = append(notes, op.done)
		}
		return protocol.Event{Status: protocol.StatusSkipped, Message: name + ": " + strings.Join(notes, ", ")}
	}
	if ctx.dryRun {
		notes := make([]string, 0, len(ops))
		for _, op := range ops {
			if op.verb != "" {
				notes = append(notes, "would "+op.done)
			} else {
				notes = append(notes, op.done)
			}
		}
		return protocol.Event{Status: protocol.StatusWouldChange, Message: name + ": " + strings.Join(notes, ", ")}
	}

	var stdout, stderr bytes.Buffer
	notes := make([]string, 0, len(ops))
	for _, op := range ops {
		if op.verb == "" {
			notes = append(notes, op.done)
			continue
		}
		if err := runCapture("systemctl", []string{op.verb, name}, &stdout, &stderr); err != nil {
			return protocol.Event{
				Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(),
				Message: fmt.Sprintf("systemctl %s %s: %v", op.verb, name, err),
			}
		}
		notes = append(notes, op.done)
	}
	return protocol.Event{
		Status: protocol.StatusChanged, Stdout: stdout.String(), Stderr: stderr.String(),
		Message: name + ": " + strings.Join(notes, ", "),
	}
}

// systemctlIs runs `systemctl is-active <name>` / `is-enabled` and returns the
// trimmed output. systemd exits non-zero for non-active/non-enabled states but
// still prints the status word — we ignore the exit code and read stdout.
func systemctlIs(verb, name string) (string, error) {
	cmd := exec.Command("systemctl", verb, name)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	_ = cmd.Run()
	return strings.TrimSpace(out.String()), nil
}

func runSupervisorService(ctx *runCtx, name, state string) protocol.Event {
	// Decide
	var verb string
	switch state {
	case "started":
		st, _ := supervisorState(name)
		if st == "RUNNING" {
			return protocol.Event{Status: protocol.StatusSkipped, Message: name + ": already RUNNING"}
		}
		verb = "start"
	case "stopped":
		st, _ := supervisorState(name)
		if st == "STOPPED" || st == "EXITED" || st == "FATAL" {
			return protocol.Event{Status: protocol.StatusSkipped, Message: name + ": already " + st}
		}
		verb = "stop"
	case "restarted":
		verb = "restart"
	case "reloaded":
		// supervisor's idiom: `update` rereads config files and starts/stops as needed.
		verb = "update"
	default:
		return protocol.Event{Status: protocol.StatusError, Message: "unknown service state: " + state}
	}

	if ctx.dryRun {
		if verb == "update" {
			return protocol.Event{Status: protocol.StatusWouldChange, Message: "would run: supervisorctl update"}
		}
		return protocol.Event{Status: protocol.StatusWouldChange, Message: fmt.Sprintf("would run: supervisorctl %s %s", verb, name)}
	}

	var stdout, stderr bytes.Buffer
	args := []string{verb}
	if verb != "update" {
		args = append(args, name)
	}
	if err := runCapture("supervisorctl", args, &stdout, &stderr); err != nil {
		return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "supervisorctl " + verb + ": " + err.Error()}
	}
	msg := name + ": " + verb
	if verb == "update" {
		msg = "supervisorctl update"
	}
	return protocol.Event{Status: protocol.StatusChanged, Stdout: stdout.String(), Stderr: stderr.String(), Message: msg}
}

// supervisorState parses `supervisorctl status <name>` output. Returns the
// state word (RUNNING, STOPPED, …) or empty if the program is not declared.
func supervisorState(name string) (string, error) {
	cmd := exec.Command("supervisorctl", "status", name)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	_ = cmd.Run()
	fields := strings.Fields(out.String())
	if len(fields) < 2 {
		return "", fmt.Errorf("unexpected supervisorctl status output: %q", out.String())
	}
	return fields[1], nil
}

func runCapture(name string, args []string, stdout, stderr *bytes.Buffer) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
