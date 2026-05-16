package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/axgrid/deploy/internal/protocol"
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
		return runSystemdService(t.ServiceName, state, t.ServiceEnabled)
	case "supervisor":
		return runSupervisorService(t.ServiceName, state)
	default:
		return protocol.Event{Status: protocol.StatusError, Message: "unknown service provider: " + provider}
	}
}

func runSystemdService(name, state string, enabled *bool) protocol.Event {
	var stdout, stderr bytes.Buffer
	changed := false
	notes := []string{}

	active, _ := systemctlIs("is-active", name)
	enabledNow, _ := systemctlIs("is-enabled", name)

	switch state {
	case "started":
		if active != "active" {
			if err := runCapture("systemctl", []string{"start", name}, &stdout, &stderr); err != nil {
				return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "systemctl start: " + err.Error()}
			}
			changed = true
			notes = append(notes, "started")
		} else {
			notes = append(notes, "already active")
		}
	case "stopped":
		if active == "active" {
			if err := runCapture("systemctl", []string{"stop", name}, &stdout, &stderr); err != nil {
				return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "systemctl stop: " + err.Error()}
			}
			changed = true
			notes = append(notes, "stopped")
		} else {
			notes = append(notes, "already inactive")
		}
	case "restarted":
		if err := runCapture("systemctl", []string{"restart", name}, &stdout, &stderr); err != nil {
			return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "systemctl restart: " + err.Error()}
		}
		changed = true
		notes = append(notes, "restarted")
	case "reloaded":
		if err := runCapture("systemctl", []string{"reload", name}, &stdout, &stderr); err != nil {
			return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "systemctl reload: " + err.Error()}
		}
		changed = true
		notes = append(notes, "reloaded")
	}

	if enabled != nil {
		switch {
		case *enabled && enabledNow != "enabled":
			if err := runCapture("systemctl", []string{"enable", name}, &stdout, &stderr); err != nil {
				return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "systemctl enable: " + err.Error()}
			}
			changed = true
			notes = append(notes, "enabled")
		case !*enabled && enabledNow == "enabled":
			if err := runCapture("systemctl", []string{"disable", name}, &stdout, &stderr); err != nil {
				return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "systemctl disable: " + err.Error()}
			}
			changed = true
			notes = append(notes, "disabled")
		}
	}

	status := protocol.StatusSkipped
	if changed {
		status = protocol.StatusChanged
	}
	return protocol.Event{
		Status:  status,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
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

func runSupervisorService(name, state string) protocol.Event {
	var stdout, stderr bytes.Buffer

	switch state {
	case "started":
		st, _ := supervisorState(name)
		if st == "RUNNING" {
			return protocol.Event{Status: protocol.StatusSkipped, Message: name + ": already RUNNING"}
		}
		if err := runCapture("supervisorctl", []string{"start", name}, &stdout, &stderr); err != nil {
			return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "supervisorctl start: " + err.Error()}
		}
		return protocol.Event{Status: protocol.StatusChanged, Stdout: stdout.String(), Stderr: stderr.String(), Message: name + ": started"}

	case "stopped":
		st, _ := supervisorState(name)
		if st == "STOPPED" || st == "EXITED" || st == "FATAL" {
			return protocol.Event{Status: protocol.StatusSkipped, Message: name + ": already " + st}
		}
		if err := runCapture("supervisorctl", []string{"stop", name}, &stdout, &stderr); err != nil {
			return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "supervisorctl stop: " + err.Error()}
		}
		return protocol.Event{Status: protocol.StatusChanged, Stdout: stdout.String(), Stderr: stderr.String(), Message: name + ": stopped"}

	case "restarted":
		if err := runCapture("supervisorctl", []string{"restart", name}, &stdout, &stderr); err != nil {
			return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "supervisorctl restart: " + err.Error()}
		}
		return protocol.Event{Status: protocol.StatusChanged, Stdout: stdout.String(), Stderr: stderr.String(), Message: name + ": restarted"}

	case "reloaded":
		// supervisor's idiom: `update` rereads config files and starts/stops as needed.
		if err := runCapture("supervisorctl", []string{"update"}, &stdout, &stderr); err != nil {
			return protocol.Event{Status: protocol.StatusError, Stdout: stdout.String(), Stderr: stderr.String(), Message: "supervisorctl update: " + err.Error()}
		}
		return protocol.Event{Status: protocol.StatusChanged, Stdout: stdout.String(), Stderr: stderr.String(), Message: "supervisorctl update"}
	}
	return protocol.Event{Status: protocol.StatusError, Message: "unknown service state: " + state}
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
