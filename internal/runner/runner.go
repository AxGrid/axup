// Package runner orchestrates a single phase (bootstrap or deploy) across one
// or more hosts: resolve inventory → render templates → split tasks into
// CLI-local + agent-remote → run local tasks once → fan out to remote hosts
// in parallel → stream events.
package runner

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/axgrid/axup/internal/agentbin"
	"github.com/axgrid/axup/internal/inventory"
	"github.com/axgrid/axup/internal/local"
	"github.com/axgrid/axup/internal/protocol"
	"github.com/axgrid/axup/internal/rulebook"
	"github.com/axgrid/axup/internal/transport"
)

type Options struct {
	Phase         string // "bootstrap", "deploy", or any custom phase declared in the rulebook — ignored when StatusOnly is true
	Host          string // user@addr literal, or inventory host name
	Group         string // inventory group name (mutex with single-host modes)
	RulebookPath  string
	InventoryPath string // optional: explicit inventory file (overrides default inventory.yaml next to rulebook). May be age-encrypted.
	VarsFile      string // optional: extra vars YAML; merged after rulebook defaults and git auto, before inventory host vars
	KeyPath       string // optional: explicit SSH private key
	Password      string // optional: explicit SSH password
	Sudo          bool   // wrap agent in sudo -H -S
	SudoPassword  string // optional: sudo password (empty = expect NOPASSWD)
	DryRun         bool // --check / --dry-run: preview only, no changes applied
	Diff           bool // --diff: in dry-run mode, agent attaches a unified diff to would_change events for copy/template
	StatusOnly     bool   // `axup status` mode: skip local tasks, ask agent to report state drift
	IncludeHistory bool   // `axup status --history`: ask the agent to attach each file's history chain to its status event
	Rollback       bool   // `axup rollback` mode: agent restores files from history instead of running tasks
	RollbackStep   int    // how many versions back to restore (1 = previous)
	RollbackTask   string // optional: restrict rollback to a single tracked path
	ClearHistory   bool   // `axup history clear` mode: agent wipes every FileState.History + removes the remote history dir
}

func Run(opts Options) error {
	recomputeColor()
	probeRb, err := rulebook.Load(opts.RulebookPath, rulebook.LoadOptions{VarsFile: opts.VarsFile})
	if err != nil {
		return err
	}
	inv, err := loadInventory(probeRb.Dir, opts.InventoryPath)
	if err != nil {
		return err
	}
	hosts, err := inv.Resolve(opts.Host, opts.Group)
	if err != nil {
		return err
	}

	if opts.StatusOnly {
		return runStatus(opts, hosts, probeRb.Name)
	}
	if opts.Rollback {
		return runRollback(opts, hosts, probeRb.Name)
	}
	if opts.ClearHistory {
		return runHistoryClear(opts, hosts, probeRb.Name)
	}

	if opts.Phase == "" {
		return fmt.Errorf("phase is required (e.g. bootstrap, deploy, or any custom phase declared in the rulebook)")
	}
	if probeRb.Phase(opts.Phase) == nil {
		return fmt.Errorf("rulebook %q has no phase %q (declared: %v)", probeRb.Name, opts.Phase, probeRb.PhaseNames())
	}

	// Local tasks (docker_build, docker_login(local)) run once for the whole
	// invocation regardless of host count. We use the FIRST host's vars to
	// build them — typical usage has these be host-invariant. If you need
	// per-host builds you'd run deploy separately per host.
	rb0, err := rulebook.Load(opts.RulebookPath, rulebook.LoadOptions{HostVars: hosts[0].Vars, VarsFile: opts.VarsFile})
	if err != nil {
		return err
	}
	tasks0 := pickPhaseTasks(rb0, opts.Phase)
	if len(tasks0) == 0 {
		fmt.Printf("rulebook %q has no tasks for phase %q\n", rb0.Name, opts.Phase)
		return nil
	}
	localTasks, _, err := buildPlans(rb0, opts.Phase, tasks0)
	if err != nil {
		return err
	}
	localSum := newSummary()
	for _, t := range localTasks {
		printEvent("[local]", protocol.Event{Type: protocol.EventTaskStart, TaskID: t.ID, Message: t.Name})
		ev := local.Execute(t, opts.DryRun)
		ev.Type = protocol.EventTaskEnd
		ev.TaskID = t.ID
		localSum.count(ev.Status)
		printEvent("[local]", ev)
		if ev.Status == protocol.StatusError {
			return fmt.Errorf("local task %q failed", t.Name)
		}
	}
	if !localSum.empty() {
		printLine("%s %s\n", cyan("[local]"), localSum.line())
	}

	// Fan out to remote hosts in parallel. errgroup cancels on first error
	// but lets in-flight goroutines finish their current task before exiting.
	g := new(errgroup.Group)
	failedAny := struct {
		mu sync.Mutex
		v  bool
	}{}
	for i := range hosts {
		host := hosts[i]
		g.Go(func() error {
			rbH := rb0
			if i != 0 {
				var err error
				rbH, err = rulebook.Load(opts.RulebookPath, rulebook.LoadOptions{HostVars: host.Vars, VarsFile: opts.VarsFile})
				if err != nil {
					return fmt.Errorf("[%s] load: %w", host.Name, err)
				}
			}
			tasks := pickPhaseTasks(rbH, opts.Phase)
			_, remoteTasks, err := buildPlans(rbH, opts.Phase, tasks)
			if err != nil {
				return fmt.Errorf("[%s] build plan: %w", host.Name, err)
			}
			if len(remoteTasks) == 0 {
				return nil
			}
			plan := protocol.Plan{
				RulebookName: rbH.Name,
				Phase:        opts.Phase,
				DryRun:       opts.DryRun,
				Diff:         opts.Diff,
				KeepHistory:  rbH.History,
				Tasks:        remoteTasks,
			}
			failed, err := runOnHost(opts, host, plan)
			if err != nil {
				return err
			}
			if failed {
				failedAny.mu.Lock()
				failedAny.v = true
				failedAny.mu.Unlock()
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if failedAny.v {
		return fmt.Errorf("one or more tasks failed")
	}
	return nil
}

// loadInventory picks the right inventory source: an explicit --inventory
// PATH (which may be age-encrypted) if set, otherwise the default
// inventory.yaml next to the rulebook (optional — nil is fine, fall back to
// ad-hoc user@addr).
func loadInventory(rulebookDir, explicit string) (*inventory.Inventory, error) {
	if explicit != "" {
		return inventory.LoadPath(explicit)
	}
	return inventory.LoadDir(rulebookDir)
}

// runStatus fans out a StatusOnly plan to every resolved host. No local tasks,
// no per-host rulebook reload (status doesn't depend on host vars — it asks
// the agent to read state.json). Errors per host don't stop the fan-out: we
// want to see every server's state even if one is unreachable.
func runStatus(opts Options, hosts []inventory.Resolved, rulebookName string) error {
	g := new(errgroup.Group)
	failedAny := struct {
		mu sync.Mutex
		v  bool
	}{}
	for i := range hosts {
		host := hosts[i]
		g.Go(func() error {
			plan := protocol.Plan{
				RulebookName:   rulebookName,
				Phase:          "status",
				StatusOnly:     true,
				IncludeHistory: opts.IncludeHistory,
			}
			failed, err := runOnHost(opts, host, plan)
			if err != nil {
				// Report but don't abort — partial status across a group is useful.
				printLine("%s %s\n", red("[error]"), err.Error())
				failedAny.mu.Lock()
				failedAny.v = true
				failedAny.mu.Unlock()
				return nil
			}
			if failed {
				failedAny.mu.Lock()
				failedAny.v = true
				failedAny.mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	if failedAny.v {
		return fmt.Errorf("status: one or more hosts had problems")
	}
	return nil
}

func pickPhaseTasks(rb *rulebook.Rulebook, phase string) []rulebook.Task {
	return rb.Phase(phase)
}

// runHistoryClear fans out a ClearHistory plan to every resolved host.
// Same fan-out shape as runStatus / runRollback. No local tasks, no
// per-host rulebook reload — the operation only touches each host's
// state.json + history dir.
func runHistoryClear(opts Options, hosts []inventory.Resolved, rulebookName string) error {
	g := new(errgroup.Group)
	failedAny := struct {
		mu sync.Mutex
		v  bool
	}{}
	for i := range hosts {
		host := hosts[i]
		g.Go(func() error {
			plan := protocol.Plan{
				RulebookName: rulebookName,
				Phase:        "history-clear",
				DryRun:       opts.DryRun,
				ClearHistory: true,
			}
			failed, err := runOnHost(opts, host, plan)
			if err != nil {
				printLine("%s %s\n", red("[error]"), err.Error())
				failedAny.mu.Lock()
				failedAny.v = true
				failedAny.mu.Unlock()
				return nil
			}
			if failed {
				failedAny.mu.Lock()
				failedAny.v = true
				failedAny.mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	if failedAny.v {
		return fmt.Errorf("history clear: one or more hosts had errors")
	}
	return nil
}

// runRollback fans out a Rollback plan to every resolved host. Mirrors
// runStatus: no local tasks, no per-host rulebook reload (rollback only
// reads state.json + history archives on the remote). Per-host errors
// don't abort the fan-out — partial visibility is more useful than none
// when one host is unreachable.
func runRollback(opts Options, hosts []inventory.Resolved, rulebookName string) error {
	g := new(errgroup.Group)
	failedAny := struct {
		mu sync.Mutex
		v  bool
	}{}
	for i := range hosts {
		host := hosts[i]
		g.Go(func() error {
			plan := protocol.Plan{
				RulebookName: rulebookName,
				Phase:        "rollback",
				DryRun:       opts.DryRun,
				Rollback:     true,
				RollbackStep: opts.RollbackStep,
				RollbackTask: opts.RollbackTask,
			}
			failed, err := runOnHost(opts, host, plan)
			if err != nil {
				printLine("%s %s\n", red("[error]"), err.Error())
				failedAny.mu.Lock()
				failedAny.v = true
				failedAny.mu.Unlock()
				return nil
			}
			if failed {
				failedAny.mu.Lock()
				failedAny.v = true
				failedAny.mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	if failedAny.v {
		return fmt.Errorf("rollback: one or more hosts had errors")
	}
	return nil
}

// runOnHost opens SSH, uploads the agent, sends the Plan, streams events.
// Returns whether any task reported status=error. Prints a per-host summary
// line after the agent's done event so the user sees totals per server.
func runOnHost(opts Options, host inventory.Resolved, plan protocol.Plan) (failed bool, err error) {
	cli, err := transport.Dial(host.Spec, transport.AuthOptions{
		KeyPath:  opts.KeyPath,
		Password: opts.Password,
	})
	if err != nil {
		return false, fmt.Errorf("[%s] %w", host.Name, err)
	}
	defer cli.Close()

	tag := fmt.Sprintf("[%s]", host.Name)
	if host.AdHoc {
		// For ad-hoc hosts, Name == raw spec, which is also informative; keep
		// the spec form so output looks like before single-host changes.
		tag = fmt.Sprintf("[%s]", cli.Host().Pretty())
	}

	arch, err := cli.DetectArch()
	if err != nil {
		return false, fmt.Errorf("[%s] %w", host.Name, err)
	}
	printLine("%s arch=%s\n", tag, arch)

	bin, err := agentbin.Binary(arch)
	if err != nil {
		return false, fmt.Errorf("[%s] %w", host.Name, err)
	}

	suffix, err := randSuffix(8)
	if err != nil {
		return false, err
	}
	remotePath := "/tmp/axupd-" + suffix
	defer func() { _, _, _ = cli.Run("rm -f " + remotePath) }()

	if err := cli.UploadBinary(bin, remotePath); err != nil {
		return false, fmt.Errorf("[%s] %w", host.Name, err)
	}
	printLine("%s agent uploaded (%d bytes → %s)\n", tag, len(bin), remotePath)

	planJSON, err := json.Marshal(plan)
	if err != nil {
		return false, err
	}

	sum := newSummary()
	err = cli.RunAgent(remotePath, planJSON, transport.AgentExec{
		Sudo:         opts.Sudo,
		SudoPassword: opts.SudoPassword,
	}, func(line []byte) {
		var ev protocol.Event
		if jerr := json.Unmarshal(line, &ev); jerr != nil {
			printLine("%s bad event line: %s\n", tag, string(line))
			return
		}
		if ev.Type == protocol.EventTaskEnd {
			sum.count(ev.Status)
		}
		if ev.Status == protocol.StatusError {
			failed = true
		}
		printEvent(tag, ev)
	})
	if err != nil {
		return false, fmt.Errorf("[%s] %w", host.Name, err)
	}
	if !sum.empty() {
		printLine("%s %s\n", cyan(tag), sum.line())
	}
	return failed, nil
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
			vars := rb.Vars
			if t.EffectiveVars != nil {
				vars = t.EffectiveVars
			}
			body, err := rb.RenderTemplate(t.Template.Src, vars)
			if err != nil {
				return nil, nil, err
			}
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name,
				Type:    protocol.TaskTemplate,
				DstPath: t.Template.Dst, Mode: t.Template.Mode,
				Sha256: sha256Hex(body), BodyB64: base64.StdEncoding.EncodeToString(body),
			})

		case "mkdir":
			mode := t.Mkdir.Mode
			if mode == "" {
				mode = "0755"
			}
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name,
				Type:       protocol.TaskMkdir,
				MkdirPath:  t.Mkdir.Path,
				Mode:       mode,
				MkdirOwner: t.Mkdir.Owner,
				MkdirGroup: t.Mkdir.Group,
			})

		case "symlink":
			force := true
			if t.Symlink.Force != nil {
				force = *t.Symlink.Force
			}
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name,
				Type:         protocol.TaskSymlink,
				SymlinkSrc:   t.Symlink.Src,
				SymlinkDst:   t.Symlink.Dst,
				SymlinkForce: force,
			})

		case "remove":
			remoteTasks = append(remoteTasks, protocol.Task{
				ID: baseID, Name: name,
				Type:            protocol.TaskRemove,
				RemovePath:      t.Remove.Path,
				RemoveRecursive: t.Remove.Recursive,
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
				ComposeWait:  t.DockerCompose.Wait,
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
			user, password, err := rb.LoadDockerCreds(t.DockerLogin)
			if err != nil {
				return nil, nil, fmt.Errorf("%s.%d (%q): %w", phase, i+1, name, err)
			}
			base := protocol.Task{
				Name:          name,
				Type:          protocol.TaskDockerLogin,
				LoginRegistry: t.DockerLogin.Registry,
				LoginUsername: user,
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

func taskName(t rulebook.Task, i int) string {
	if t.Name != "" {
		return t.Name
	}
	return fmt.Sprintf("task #%d", i+1)
}

// printLine + printEvent share a single mutex so concurrent host goroutines
// don't tear each other's output lines. Tag prefix lets the reader still
// follow each host's stream interleaved.
var printMu sync.Mutex

func printLine(format string, a ...any) {
	printMu.Lock()
	defer printMu.Unlock()
	fmt.Printf(format, a...)
}

func printEvent(tag string, ev protocol.Event) {
	printMu.Lock()
	defer printMu.Unlock()
	coloredTag := cyan(tag)
	switch ev.Type {
	case protocol.EventTaskStart:
		fmt.Printf("%s %s %s %s\n", coloredTag, bold("▶"), ev.Message, gray("("+ev.TaskID+")"))
	case protocol.EventTaskEnd:
		mark, status := paintByStatus(ev.Status)
		extra := ""
		if ev.Path != "" {
			extra = " " + gray("path="+ev.Path)
		}
		fmt.Printf("%s   %s status=%s%s\n", coloredTag, mark, status, extra)
		if s := trimTail(ev.Stdout); s != "" {
			fmt.Printf("%s     %s %s\n", coloredTag, gray("stdout:"), s)
		}
		if s := trimTail(ev.Stderr); s != "" {
			fmt.Printf("%s     %s %s\n", coloredTag, gray("stderr:"), s)
		}
		if ev.Message != "" && ev.Status != protocol.StatusSkipped {
			fmt.Printf("%s     %s %s\n", coloredTag, gray("msg:"), ev.Message)
		}
		if ev.Diff != "" {
			fmt.Printf("%s     %s\n", coloredTag, gray("diff:"))
			for _, line := range strings.Split(ev.Diff, "\n") {
				fmt.Printf("%s       %s\n", coloredTag, paintDiffLine(line))
			}
		}
		if len(ev.History) > 0 {
			fmt.Printf("%s     %s (%d versions, newest first)\n", coloredTag, gray("history:"), len(ev.History))
			for i, h := range ev.History {
				phase := ""
				if h.Phase != "" {
					phase = " phase=" + h.Phase
				}
				fmt.Printf("%s       %s  sha=%s mode=%s recorded_at=%s%s\n",
					coloredTag, gray(fmt.Sprintf("[%d]", i+1)),
					shortSha(h.Sha256), h.Mode, h.RecordedAt, phase)
			}
		}
	case protocol.EventLog:
		fmt.Printf("%s   %s %s\n", coloredTag, gray("·"), ev.Message)
	case protocol.EventDone:
		fmt.Printf("%s %s\n", coloredTag, gray("done"))
	}
}

// paintByStatus returns the mark+status string already wrapped in the
// appropriate ANSI color.
func paintByStatus(status string) (mark, statusColored string) {
	switch status {
	case protocol.StatusError:
		return red("✗"), red(status)
	case protocol.StatusSkipped:
		return gray("·"), gray(status)
	case protocol.StatusWouldChange:
		return yellow("≈"), yellow(status)
	case protocol.StatusInSync:
		return green("✓"), green(status)
	case protocol.StatusDrift:
		return yellow("⚠"), yellow(status)
	case protocol.StatusMissing:
		return red("⊘"), red(status)
	default: // changed / ok
		return green("✓"), green(status)
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

func shortSha(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

