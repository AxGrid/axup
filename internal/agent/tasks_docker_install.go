package agent

import (
	"bytes"
	"os/exec"

	"github.com/axgrid/axup/internal/protocol"
)

// runDockerInstallTask installs Docker Engine via the official get.docker.com
// convenience script. Idempotent on the "binary on PATH" check — if `docker
// --version` already succeeds we skip the download entirely.
//
// We deliberately don't gate on docker daemon health here; bringing the daemon
// up is the service task's job. Install + start are separate concerns.
func runDockerInstallTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if dockerOnPath() {
		return protocol.Event{Status: protocol.StatusSkipped, Message: "docker already installed"}
	}

	if ctx.dryRun {
		return protocol.Event{
			Status:  protocol.StatusWouldChange,
			Message: "would run: curl -fsSL https://get.docker.com | sh",
		}
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("/bin/sh", "-c", "curl -fsSL https://get.docker.com | sh")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return protocol.Event{
			Status:  protocol.StatusError,
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Message: "curl get.docker.com | sh: " + err.Error(),
		}
	}
	if !dockerOnPath() {
		return protocol.Event{
			Status:  protocol.StatusError,
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Message: "install script finished but `docker` is not on PATH",
		}
	}
	return protocol.Event{
		Status:  protocol.StatusChanged,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
		Message: "docker installed via get.docker.com",
	}
}

func dockerOnPath() bool {
	return exec.Command("docker", "--version").Run() == nil
}
