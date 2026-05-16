package agent

import (
	"bytes"
	"fmt"
	"os/exec"

	"github.com/axgrid/deploy/internal/protocol"
)

// runDockerComposeTask wraps `docker compose -f <dir>/docker-compose.yml <action>`.
// For up/down/restart we always invoke compose — docker is idempotent on its
// side, but we report status=changed every time because parsing compose ps
// reliably across versions adds complexity we don't need for the MVP.
func runDockerComposeTask(ctx *runCtx, t protocol.Task) protocol.Event {
	state := t.ComposeState
	if state == "" {
		state = "up"
	}

	var allStdout, allStderr bytes.Buffer

	run := func(args ...string) error {
		cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
		cmd.Dir = t.ComposeDir
		cmd.Stdout = &allStdout
		cmd.Stderr = &allStderr
		return cmd.Run()
	}

	if t.ComposePull && (state == "up" || state == "restarted") {
		if err := run("pull"); err != nil {
			return protocol.Event{
				Status:  protocol.StatusError,
				Stdout:  allStdout.String(),
				Stderr:  allStderr.String(),
				Message: "docker compose pull: " + err.Error(),
			}
		}
	}

	var action []string
	switch state {
	case "up":
		action = []string{"up", "-d", "--remove-orphans"}
	case "down":
		action = []string{"down"}
	case "restarted":
		action = []string{"restart"}
	case "pulled":
		action = []string{"pull"}
	default:
		return protocol.Event{Status: protocol.StatusError, Message: "unknown compose state: " + state}
	}

	if err := run(action...); err != nil {
		return protocol.Event{
			Status:  protocol.StatusError,
			Stdout:  allStdout.String(),
			Stderr:  allStderr.String(),
			Message: fmt.Sprintf("docker compose %s: %v", action[0], err),
		}
	}
	return protocol.Event{
		Status:  protocol.StatusChanged,
		Stdout:  allStdout.String(),
		Stderr:  allStderr.String(),
		Path:    t.ComposeDir,
		Message: fmt.Sprintf("docker compose %s", action[0]),
	}
}
