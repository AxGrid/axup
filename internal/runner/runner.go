// Package runner orchestrates a single phase (bootstrap or deploy): load
// rulebook → render templates → split tasks into CLI-local + agent-remote →
// run local tasks → upload agent → stream events for remote tasks.
package runner

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/axgrid/deploy/internal/agentbin"
	"github.com/axgrid/deploy/internal/local"
	"github.com/axgrid/deploy/internal/protocol"
	"github.com/axgrid/deploy/internal/rulebook"
	"github.com/axgrid/deploy/internal/transport"
)

type Options struct {
	Phase        string // "bootstrap" or "deploy"
	Host         string // "[user@]addr[:port]"
	RulebookPath string
	KeyPath      string // optional: explicit SSH private key
	Password     string // optional: explicit SSH password
	Sudo         bool   // wrap agent in sudo -H -S
	SudoPassword string // optional: sudo password (empty = expect NOPASSWD)
}

func Run(opts Options) error {
	rb, err := rulebook.Load(opts.RulebookPath)
	if err != nil {
		return err
	}

	var tasks []rulebook.Task
	switch opts.Phase {
	case "bootstrap":
		tasks = rb.Bootstrap
	case "deploy":
		tasks = rb.Deploy
	default:
		return fmt.Errorf("unknown phase %q", opts.Phase)
	}
	if len(tasks) == 0 {
		fmt.Printf("rulebook %q has no tasks for phase %q\n", rb.Name, opts.Phase)
		return nil
	}

	localTasks, remoteTasks, err := buildPlans(rb, opts.Phase, tasks)
	if err != nil {
		return err
	}

	// Local tasks run first regardless of declared position. In practice the
	// only local tasks are docker_build and docker_login(local) — both must
	// complete before the remote does compose pull / image-bearing template,
	// so the ordering is correct for the common case.
	failed := false
	for _, t := range localTasks {
		printEvent("[local]", protocol.Event{Type: protocol.EventTaskStart, TaskID: t.ID, Message: t.Name})
		ev := local.Execute(t)
		ev.Type = protocol.EventTaskEnd
		ev.TaskID = t.ID
		printEvent("[local]", ev)
		if ev.Status == protocol.StatusError {
			failed = true
			return fmt.Errorf("local task %q failed", t.Name)
		}
	}

	if len(remoteTasks) == 0 {
		if failed {
			return fmt.Errorf("one or more tasks failed")
		}
		return nil
	}

	cli, err := transport.Dial(opts.Host, transport.AuthOptions{
		KeyPath:  opts.KeyPath,
		Password: opts.Password,
	})
	if err != nil {
		return err
	}
	defer cli.Close()

	tag := fmt.Sprintf("[%s]", cli.Host().Pretty())

	arch, err := cli.DetectArch()
	if err != nil {
		return err
	}
	fmt.Printf("%s arch=%s\n", tag, arch)

	bin, err := agentbin.Binary(arch)
	if err != nil {
		return err
	}

	suffix, err := randSuffix(8)
	if err != nil {
		return err
	}
	remotePath := "/tmp/deployd-" + suffix
	defer func() {
		_, _, _ = cli.Run("rm -f " + remotePath)
	}()

	if err := cli.UploadBinary(bin, remotePath); err != nil {
		return err
	}
	fmt.Printf("%s agent uploaded (%d bytes → %s)\n", tag, len(bin), remotePath)

	plan := protocol.Plan{
		RulebookName: rb.Name,
		Phase:        opts.Phase,
		Tasks:        remoteTasks,
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return err
	}

	err = cli.RunAgent(remotePath, planJSON, transport.AgentExec{
		Sudo:         opts.Sudo,
		SudoPassword: opts.SudoPassword,
	}, func(line []byte) {
		var ev protocol.Event
		if jerr := json.Unmarshal(line, &ev); jerr != nil {
			fmt.Fprintf(os.Stderr, "%s bad event line: %s\n", tag, string(line))
			return
		}
		if ev.Status == protocol.StatusError {
			failed = true
		}
		printEvent(tag, ev)
	})
	if err != nil {
		return err
	}
	if failed {
		return fmt.Errorf("one or more tasks failed")
	}
	return nil
}

// buildPlans walks the rulebook's task list and emits two slices: local tasks
// to run on the CLI host, and remote tasks to send to the agent. Most tasks go
// only to one slice; docker_login with location=both fans out to both.
func buildPlans(rb *rulebook.Rulebook, phase string, tasks []rulebook.Task) (localTasks, remoteTasks []protocol.Task, err error) {
	for i, t := range tasks {
		baseID := fmt.Sprintf("%s.%d", phase, i+1)
		name := taskName(t, i)
		whenChanged := t.WhenChanged

		switch t.Kind() {
		case "command":
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name, WhenChanged: whenChanged,
				Type: protocol.TaskCommand, Command: t.Command,
			})

		case "copy":
			body, err := rb.ReadFile(t.Copy.Src)
			if err != nil {
				return nil, nil, err
			}
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name,
				Type:    protocol.TaskCopy,
				DstPath: t.Copy.Dst, Mode: t.Copy.Mode,
				Sha256: sha256Hex(body), BodyB64: base64.StdEncoding.EncodeToString(body),
			})

		case "template":
			body, err := rb.RenderTemplate(t.Template.Src, rb.Vars)
			if err != nil {
				return nil, nil, err
			}
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name,
				Type:    protocol.TaskTemplate,
				DstPath: t.Template.Dst, Mode: t.Template.Mode,
				Sha256: sha256Hex(body), BodyB64: base64.StdEncoding.EncodeToString(body),
			})

		case "apt":
			state := t.Apt.State
			if state == "" {
				state = "present"
			}
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name, WhenChanged: whenChanged,
				Type:           protocol.TaskApt,
				AptPackages:    append([]string{}, t.Apt.Name...),
				AptState:       state,
				AptUpdateCache: t.Apt.UpdateCache,
			})

		case "service":
			state := t.Service.State
			if state == "" {
				state = "started"
			}
			provider := t.Service.Provider
			if provider == "" {
				provider = "systemd"
			}
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name, WhenChanged: whenChanged,
				Type:            protocol.TaskService,
				ServiceName:     t.Service.Name,
				ServiceState:    state,
				ServiceEnabled:  t.Service.Enabled,
				ServiceProvider: provider,
			})

		case "docker_compose":
			state := t.DockerCompose.State
			if state == "" {
				state = "up"
			}
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name, WhenChanged: whenChanged,
				Type:         protocol.TaskDockerCompose,
				ComposeDir:   t.DockerCompose.Dir,
				ComposeState: state,
				ComposePull:  t.DockerCompose.Pull,
			})

		case "docker_install":
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name, WhenChanged: whenChanged,
				Type: protocol.TaskDockerInstall,
			})

		case "docker_build":
			tags := t.DockerBuild.Tags
			if t.DockerBuild.Tag != "" {
				tags = []string{t.DockerBuild.Tag}
			}
			ctxPath := t.DockerBuild.Context
			if !filepath.IsAbs(ctxPath) {
				ctxPath = filepath.Join(rb.Dir, ctxPath)
			}
			localTasks = append(localTasks, protocol.Task{
				ID: baseID, Name: name,
				Type:            protocol.TaskDockerBuild,
				BuildContext:    ctxPath,
				BuildDockerfile: t.DockerBuild.Dockerfile,
				BuildTags:       tags,
				BuildPush:       t.DockerBuild.Push,
				BuildPlatform:   t.DockerBuild.Platform,
				BuildArgs:       t.DockerBuild.BuildArgs,
			})

		case "docker_login":
			password, err := resolveLoginPassword(rb, t.DockerLogin)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.%d (%q): %w", phase, i+1, name, err)
			}
			base := protocol.Task{
				Name:          name,
				Type:          protocol.TaskDockerLogin,
				LoginRegistry: t.DockerLogin.Registry,
				LoginUsername: t.DockerLogin.Username,
				LoginPassword: password,
			}
			loc := t.DockerLogin.Location
			if loc == "" {
				loc = "both"
			}
			if loc == "local" || loc == "both" {
				lt := base
				lt.ID = baseID + ".local"
				localTasks = append(localTasks, lt)
			}
			if loc == "remote" || loc == "both" {
				rt := base
				rt.ID = baseID + ".remote"
				remoteTasks = append(remoteTasks, rt)
			}
		}
	}
	return localTasks, remoteTasks, nil
}

func resolveLoginPassword(rb *rulebook.Rulebook, spec *rulebook.DockerLoginSpec) (string, error) {
	switch {
	case spec.Password != "":
		return spec.Password, nil
	case spec.PasswordFile != "":
		path := spec.PasswordFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(rb.Dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read password_file: %w", err)
		}
		// Trim trailing newline that editors add — most registries reject it.
		for len(data) > 0 && (data[len(data)-1] == '\n' || data[len(data)-1] == '\r') {
			data = data[:len(data)-1]
		}
		return string(data), nil
	case spec.PasswordEnv != "":
		v := os.Getenv(spec.PasswordEnv)
		if v == "" {
			return "", fmt.Errorf("env var %s is empty or unset", spec.PasswordEnv)
		}
		return v, nil
	}
	return "", fmt.Errorf("no password source set")
}

func taskName(t rulebook.Task, i int) string {
	if t.Name != "" {
		return t.Name
	}
	return fmt.Sprintf("task #%d", i+1)
}

func printEvent(tag string, ev protocol.Event) {
	switch ev.Type {
	case protocol.EventTaskStart:
		fmt.Printf("%s ▶ %s (%s)\n", tag, ev.Message, ev.TaskID)
	case protocol.EventTaskEnd:
		mark := "✓"
		switch ev.Status {
		case protocol.StatusError:
			mark = "✗"
		case protocol.StatusSkipped:
			mark = "·"
		}
		extra := ""
		if ev.Path != "" {
			extra = " path=" + ev.Path
		}
		fmt.Printf("%s   %s status=%s%s\n", tag, mark, ev.Status, extra)
		if s := trimTail(ev.Stdout); s != "" {
			fmt.Printf("%s     stdout: %s\n", tag, s)
		}
		if s := trimTail(ev.Stderr); s != "" {
			fmt.Printf("%s     stderr: %s\n", tag, s)
		}
		if ev.Message != "" && ev.Status != protocol.StatusSkipped {
			fmt.Printf("%s     msg: %s\n", tag, ev.Message)
		}
	case protocol.EventLog:
		fmt.Printf("%s   · %s\n", tag, ev.Message)
	case protocol.EventDone:
		fmt.Printf("%s done\n", tag)
	}
}

func trimTail(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func randSuffix(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
