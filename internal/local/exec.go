// Package local runs Plan tasks that must execute on the CLI host: docker_build
// (so the image is built on the developer's machine and pushed to the registry
// the remote can pull from) and docker_login (so the build can push and the
// remote can pull from a private registry).
package local

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/axgrid/deploy/internal/protocol"
)

// Execute runs a CLI-local task. Output streams to os.Stdout/Stderr so the
// user sees buildx progress live; the returned Event is a summary.
func Execute(t protocol.Task) protocol.Event {
	switch t.Type {
	case protocol.TaskDockerBuild:
		return runDockerBuild(t)
	case protocol.TaskDockerLogin:
		return runLocalDockerLogin(t)
	default:
		return protocol.Event{
			Status:  protocol.StatusError,
			Message: "local executor cannot handle task type: " + t.Type,
		}
	}
}

func runDockerBuild(t protocol.Task) protocol.Event {
	if _, err := exec.LookPath("docker"); err != nil {
		return protocol.Event{Status: protocol.StatusError, Message: "docker not on PATH"}
	}
	dockerfile := t.BuildDockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	platform := t.BuildPlatform
	if platform == "" {
		platform = "linux/amd64"
	}

	args := []string{"buildx", "build", "--platform", platform, "-f", dockerfile}
	for _, tag := range t.BuildTags {
		args = append(args, "-t", tag)
	}
	for k, v := range t.BuildArgs {
		args = append(args, "--build-arg", k+"="+v)
	}
	if t.BuildPush {
		args = append(args, "--push")
	} else {
		// --load makes the image available to the local docker daemon for
		// inspection / non-pushed smoke tests. Buildx default is to throw the
		// result away unless --push or --load is set.
		args = append(args, "--load")
	}
	args = append(args, t.BuildContext)

	cmd := exec.Command("docker", args...)
	// Stream live so users see real progress, but also capture a tail for the
	// Event so the failure mode is debuggable.
	var tail bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &tail)
	cmd.Stderr = io.MultiWriter(os.Stderr, &tail)
	fmt.Fprintf(os.Stderr, "  docker %s\n", strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return protocol.Event{
			Status:  protocol.StatusError,
			Stderr:  truncTail(tail.String()),
			Message: "docker buildx build: " + err.Error(),
		}
	}
	suffix := ""
	if t.BuildPush {
		suffix = " (pushed)"
	}
	return protocol.Event{
		Status:  protocol.StatusChanged,
		Message: fmt.Sprintf("built %s%s", strings.Join(t.BuildTags, ", "), suffix),
	}
}

func runLocalDockerLogin(t protocol.Task) protocol.Event {
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

func truncTail(s string) string {
	const limit = 2048
	if len(s) <= limit {
		return s
	}
	return "…(truncated)…\n" + s[len(s)-limit:]
}
