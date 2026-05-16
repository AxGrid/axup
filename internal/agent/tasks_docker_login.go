package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/axgrid/deploy/internal/protocol"
)

func runDockerLoginTask(ctx *runCtx, t protocol.Task) protocol.Event {
	cmd := exec.Command("docker", "login", "-u", t.LoginUsername, "--password-stdin", t.LoginRegistry)
	cmd.Stdin = strings.NewReader(t.LoginPassword)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return protocol.Event{
			Status:  protocol.StatusError,
			Stdout:  stdout.String(),
			Stderr:  stderr.String(),
			Message: fmt.Sprintf("docker login %s: %v", t.LoginRegistry, err),
		}
	}
	return protocol.Event{
		Status:  protocol.StatusChanged,
		Stdout:  strings.TrimSpace(stdout.String()),
		Stderr:  strings.TrimSpace(stderr.String()),
		Message: "logged in to " + t.LoginRegistry + " as " + t.LoginUsername,
	}
}
