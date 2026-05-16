package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/axgrid/deploy/internal/protocol"
)

// runAptTask installs or removes packages via apt-get. Pre-checks with
// dpkg-query so an already-converged system reports status=skipped rather than
// invoking apt-get for nothing.
func runAptTask(ctx *runCtx, t protocol.Task) protocol.Event {
	state := t.AptState
	if state == "" {
		state = "present"
	}

	var todo []string
	for _, pkg := range t.AptPackages {
		installed, err := dpkgInstalled(pkg)
		if err != nil {
			return protocol.Event{Status: protocol.StatusError, Message: "dpkg-query: " + err.Error()}
		}
		switch state {
		case "present":
			if !installed {
				todo = append(todo, pkg)
			}
		case "absent":
			if installed {
				todo = append(todo, pkg)
			}
		}
	}

	if !t.AptUpdateCache && len(todo) == 0 {
		return protocol.Event{Status: protocol.StatusSkipped, Message: "all packages already in desired state"}
	}

	if ctx.dryRun {
		verb := "install"
		if state == "absent" {
			verb = "remove"
		}
		msg := fmt.Sprintf("would %s %d package(s): %s", verb, len(todo), strings.Join(todo, ", "))
		if len(todo) == 0 {
			msg = "would run apt-get update (no packages to install/remove)"
		}
		return protocol.Event{Status: protocol.StatusWouldChange, Message: msg}
	}

	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	var stdout, stderr bytes.Buffer

	if t.AptUpdateCache {
		cmd := exec.Command("apt-get", "update", "-qq")
		cmd.Env = append(cmd.Environ(), env...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return protocol.Event{
				Status:  protocol.StatusError,
				Stdout:  stdout.String(),
				Stderr:  stderr.String(),
				Message: "apt-get update: " + err.Error(),
			}
		}
	}

	if len(todo) == 0 {
		return protocol.Event{
			Status:  protocol.StatusChanged,
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Message: "apt-get update completed; all packages already in desired state",
		}
	}

	args := []string{"install", "-y", "--no-install-recommends"}
	if state == "absent" {
		args = []string{"remove", "-y"}
	}
	args = append(args, todo...)
	cmd := exec.Command("apt-get", args...)
	cmd.Env = append(cmd.Environ(), env...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return protocol.Event{
			Status:  protocol.StatusError,
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Message: fmt.Sprintf("apt-get %s: %v", args[0], err),
		}
	}

	return protocol.Event{
		Status:  protocol.StatusChanged,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Message: fmt.Sprintf("%sed %d package(s): %s", strings.TrimSuffix(args[0], "e"), len(todo), strings.Join(todo, ", ")),
	}
}

// dpkgInstalled returns true if the package is installed (status "install ok
// installed"). A non-zero exit from dpkg-query is treated as "not installed".
func dpkgInstalled(pkg string) (bool, error) {
	cmd := exec.Command("dpkg-query", "-W", "-f=${Status}", pkg)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		// dpkg-query exits 1 for "not installed" — that's not a hard error.
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, fmt.Errorf("%w: %s", err, errBuf.String())
	}
	return strings.Contains(out.String(), "install ok installed"), nil
}
