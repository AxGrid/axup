package rulebook

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// phaseNameRe is intentionally stricter than nameRe — phase names are
// CLI arguments (`axup run <phase>`) and YAML keys, so lower-snake
// keeps them shell-friendly and consistent with bootstrap/deploy.
var phaseNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// loadVarsFile reads a YAML file containing a top-level dict and returns
// it as map[string]any. Relative paths resolve like every other CLI
// flag — against CWD. If that doesn't exist, fall back to looking
// alongside the rulebook so `--vars vars.yaml` works when the file
// sits next to rulebook.yaml regardless of where the CLI is invoked.
func loadVarsFile(path, rulebookDir string) (map[string]any, error) {
	resolved := path
	if !filepath.IsAbs(path) {
		if _, err := os.Stat(path); err != nil {
			alt := filepath.Join(rulebookDir, path)
			if _, err2 := os.Stat(alt); err2 == nil {
				resolved = alt
			}
		}
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read --vars %s: %w", path, err)
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse --vars %s: %w", resolved, err)
	}
	return out, nil
}

func validatePhaseNames(phases map[string][]Task) error {
	for name := range phases {
		if !phaseNameRe.MatchString(name) {
			return fmt.Errorf("phase %q is not a valid identifier (must match %s)", name, phaseNameRe)
		}
		if _, reserved := reservedPhaseNames[name]; reserved {
			return fmt.Errorf("phase %q is reserved by the CLI; pick a different name", name)
		}
	}
	return nil
}

// LoadOptions is variadic — most callers pass none. Var merge precedence
// (highest wins):
//
//   HostVars (per-host inventory)
//     > VarsFile (--vars external YAML)
//       > git auto-vars (git_sha, git_short_sha, git_branch, git_dirty)
//         > rulebook vars: defaults
//
// All four merges happen during Load so the resulting Rulebook is fully
// host-specific by the time deps/expand/render run.
type LoadOptions struct {
	HostVars map[string]any
	VarsFile string // optional: path to a YAML dict of extra vars
}

func Load(path string, opts ...LoadOptions) (*Rulebook, error) {
	var o LoadOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return loadInternal(path, o)
}

func loadInternal(path string, o LoadOptions) (*Rulebook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rulebook %s: %w", path, err)
	}
	var rb Rulebook
	if err := yaml.Unmarshal(data, &rb); err != nil {
		return nil, fmt.Errorf("parse rulebook %s: %w", path, err)
	}
	if rb.Name == "" {
		return nil, fmt.Errorf("rulebook %s: missing required field 'name'", path)
	}
	if !nameRe.MatchString(rb.Name) {
		return nil, fmt.Errorf("rulebook %s: name %q must be 1-63 chars of [A-Za-z0-9._-] starting with alnum (used as a directory name in remote state)", path, rb.Name)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	rb.Dir = filepath.Dir(abs)

	// Auto-discover git facts and merge them into Vars (user-supplied vars
	// take precedence — overriding git_sha is a legitimate use case).
	auto := gitVars(rb.Dir)
	if rb.Vars == nil {
		rb.Vars = map[string]any{}
	}
	for k, v := range auto {
		if _, taken := rb.Vars[k]; !taken {
			rb.Vars[k] = v
		}
	}

	// --vars file — merged after rulebook defaults and git auto so it can
	// override either, but before HostVars so per-host inventory still wins.
	// File path is resolved relative to the rulebook's directory when
	// non-absolute, so `--vars vars.yaml` next to the rulebook just works.
	if o.VarsFile != "" {
		extra, err := loadVarsFile(o.VarsFile, rb.Dir)
		if err != nil {
			return nil, err
		}
		for k, v := range extra {
			rb.Vars[k] = v
		}
	}
	// Apply per-host vars last so they override the rulebook's defaults and
	// any auto-vars. This is what makes inventory-driven multi-host work:
	// each host's Load() produces a Rulebook tailored to that host's view.
	for k, v := range o.HostVars {
		rb.Vars[k] = v
	}

	// Reject any phase name that would collide with the CLI's reserved
	// dispatch words, and any name that's not a valid CLI argument.
	if err := validatePhaseNames(rb.Phases); err != nil {
		return nil, fmt.Errorf("rulebook %s: %w", path, err)
	}

	// `history: N` caps the per-file rollback chain on the remote. Cap at
	// 50 — anything higher is almost certainly a typo (and disk overhead
	// scales linearly with N × tracked-file size).
	if rb.History < 0 || rb.History > 50 {
		return nil, fmt.Errorf("rulebook %s: history: must be 0..50 (got %d)", path, rb.History)
	}

	// Catch the legacy module-form mistake before downstream code does:
	// a module rulebook uses `tasks:` and nothing else.
	if len(rb.Tasks) > 0 && len(rb.Phases) > 0 {
		return nil, fmt.Errorf("rulebook %s: cannot mix module-form `tasks:` with phase keys %v", path, rb.PhaseNames())
	}

	// Resolve deps and inline every `use:` task before we run downstream
	// validation. From here on the task tree is fully concrete — no further
	// modules will be loaded. Two flavors of `use:` are handled here:
	//   - git-dep refs (`use: <dep>/<module>`) — need ctx.depPaths populated
	//     by deps resolver
	//   - local refs (`use: ./<path>[/<phase>]`) — resolved relative to
	//     rb.Dir, work with or without deps[]
	// Therefore we run expandUseTasks unconditionally; the deps resolver
	// only fires when deps: is non-empty.
	var depPaths map[string]string
	if len(rb.Deps) > 0 {
		var err error
		depPaths, err = resolveDepsForLoad(&rb)
		if err != nil {
			return nil, err
		}
	}
	for name, tasks := range rb.Phases {
		ctx := &expandCtx{
			callerDir:  rb.Dir,
			depPaths:   depPaths,
			callerVars: rb.Vars,
			visited:    map[string]bool{},
			phase:      name,
		}
		expanded, err := expandUseTasks(tasks, ctx)
		if err != nil {
			return nil, fmt.Errorf("phase %q: %w", name, err)
		}
		rb.Phases[name] = expanded
	}

	for _, name := range rb.PhaseNames() {
		if err := validateTasks(name, rb.Phases[name]); err != nil {
			return nil, err
		}
	}
	if err := rb.expandStringFields(); err != nil {
		return nil, err
	}
	return &rb, nil
}

// resolveDepsForLoad reconciles the rulebook's deps[] against axup.lock,
// clones any missing repos to the cache, and returns dep_name → local_path.
// If reconciliation discovered a missing/new dep, the lock is written back so
// the next run is fully reproducible.
func resolveDepsForLoad(rb *Rulebook) (map[string]string, error) {
	lock, err := LoadLock(rb.Dir)
	if err != nil {
		return nil, err
	}
	resolved, changed, newLock, err := Reconcile(rb.Deps, lock)
	if err != nil {
		return nil, err
	}
	cacheDir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	for _, d := range rb.Deps {
		sha, ok := resolved[d.Name]
		if !ok {
			return nil, fmt.Errorf("dep %q did not resolve", d.Name)
		}
		local, err := EnsureClone(d.NormalizedURL(), sha, cacheDir)
		if err != nil {
			return nil, fmt.Errorf("clone dep %q: %w", d.Name, err)
		}
		paths[d.Name] = local
	}
	if changed {
		if err := SaveLock(rb.Dir, newLock); err != nil {
			return nil, fmt.Errorf("write %s: %w", LockFileName, err)
		}
	}
	return paths, nil
}

func validateTasks(phase string, tasks []Task) error {
	for i, t := range tasks {
		set := 0
		if t.Command != nil {
			set++
			if strings.TrimSpace(t.Command.Run) == "" {
				return fmt.Errorf("%s[%d] (%q): command.run is required", phase, i, t.Name)
			}
			if t.Command.When != "" && t.Command.Unless != "" {
				return fmt.Errorf("%s[%d] (%q): command.when and command.unless are mutually exclusive", phase, i, t.Name)
			}
		}
		if t.Copy != nil {
			set++
		}
		if t.Template != nil {
			set++
		}
		if t.Mkdir != nil {
			set++
		}
		if t.Symlink != nil {
			set++
		}
		if t.Remove != nil {
			set++
		}
		if t.User != nil {
			set++
		}
		if t.Group != nil {
			set++
		}
		if t.Chmod != nil {
			set++
		}
		if t.Chown != nil {
			set++
		}
		if t.Download != nil {
			set++
		}
		if t.Apt != nil {
			set++
		}
		if t.Service != nil {
			set++
		}
		if t.DockerCompose != nil {
			set++
		}
		if t.DockerInstall != nil {
			set++
		}
		if t.DockerBuild != nil {
			set++
		}
		if t.DockerLogin != nil {
			set++
		}
		if t.Use != "" {
			// `use:` must not survive past Load — if we see it here, expansion
			// failed or wasn't run (e.g. no deps: declared).
			return fmt.Errorf("%s[%d] (%q): use %q did not resolve — declare the dep in deps:[]", phase, i, t.Name, t.Use)
		}
		if set == 0 {
			return fmt.Errorf("%s[%d] (%q): no operation set; expected one of command/copy/template/mkdir/symlink/remove/user/group/chmod/chown/download/apt/service/docker_compose/docker_install/docker_build/docker_login/use", phase, i, t.Name)
		}
		if set > 1 {
			return fmt.Errorf("%s[%d] (%q): multiple operations set; choose exactly one", phase, i, t.Name)
		}
		if t.Copy != nil && (t.Copy.Src == "" || t.Copy.Dst == "") {
			return fmt.Errorf("%s[%d] (%q): copy requires both src and dst", phase, i, t.Name)
		}
		if t.Template != nil && (t.Template.Src == "" || t.Template.Dst == "") {
			return fmt.Errorf("%s[%d] (%q): template requires both src and dst", phase, i, t.Name)
		}
		if t.Mkdir != nil {
			if t.Mkdir.Path == "" {
				return fmt.Errorf("%s[%d] (%q): mkdir requires path (use shorthand `mkdir: /path` or full `mkdir: { path: /path }`)", phase, i, t.Name)
			}
			if t.Mkdir.Mode != "" {
				if _, err := strconv.ParseUint(t.Mkdir.Mode, 8, 32); err != nil {
					return fmt.Errorf("%s[%d] (%q): mkdir mode %q is not a valid octal string (e.g. \"0755\")", phase, i, t.Name, t.Mkdir.Mode)
				}
			}
		}
		if t.Symlink != nil {
			if t.Symlink.Src == "" || t.Symlink.Dst == "" {
				return fmt.Errorf("%s[%d] (%q): symlink requires both src (target) and dst (link path)", phase, i, t.Name)
			}
		}
		if t.Remove != nil && t.Remove.Path == "" {
			return fmt.Errorf("%s[%d] (%q): remove requires path (use shorthand `remove: /path` or full `remove: { path: /path }`)", phase, i, t.Name)
		}
		if t.User != nil {
			if t.User.Name == "" {
				return fmt.Errorf("%s[%d] (%q): user requires name (use shorthand `user: app` or full `user: { name: app }`)", phase, i, t.Name)
			}
			switch t.User.State {
			case "", "present", "absent":
			default:
				return fmt.Errorf("%s[%d] (%q): user state must be 'present' or 'absent', got %q", phase, i, t.Name, t.User.State)
			}
			if t.User.State == "absent" && (t.User.Shell != "" || t.User.Home != "" || t.User.CreateHome || len(t.User.Groups) > 0) {
				return fmt.Errorf("%s[%d] (%q): user state=absent rejects shell/home/create_home/groups (they only matter on create)", phase, i, t.Name)
			}
		}
		if t.Group != nil {
			if t.Group.Name == "" {
				return fmt.Errorf("%s[%d] (%q): group requires name (use shorthand `group: docker` or full `group: { name: docker }`)", phase, i, t.Name)
			}
			switch t.Group.State {
			case "", "present", "absent":
			default:
				return fmt.Errorf("%s[%d] (%q): group state must be 'present' or 'absent', got %q", phase, i, t.Name, t.Group.State)
			}
		}
		if t.Chmod != nil {
			if t.Chmod.Path == "" {
				return fmt.Errorf("%s[%d] (%q): chmod requires path", phase, i, t.Name)
			}
			if t.Chmod.Mode == "" {
				return fmt.Errorf("%s[%d] (%q): chmod requires mode (e.g. \"0644\")", phase, i, t.Name)
			}
			if _, err := strconv.ParseUint(t.Chmod.Mode, 8, 32); err != nil {
				return fmt.Errorf("%s[%d] (%q): chmod mode %q is not a valid octal string", phase, i, t.Name, t.Chmod.Mode)
			}
		}
		if t.Chown != nil {
			if t.Chown.Path == "" {
				return fmt.Errorf("%s[%d] (%q): chown requires path", phase, i, t.Name)
			}
			if t.Chown.Owner == "" && t.Chown.Group == "" {
				return fmt.Errorf("%s[%d] (%q): chown requires at least one of owner/group", phase, i, t.Name)
			}
		}
		if t.Download != nil {
			if t.Download.URL == "" || t.Download.Dst == "" {
				return fmt.Errorf("%s[%d] (%q): download requires both url and dst", phase, i, t.Name)
			}
			if t.Download.Mode != "" {
				if _, err := strconv.ParseUint(t.Download.Mode, 8, 32); err != nil {
					return fmt.Errorf("%s[%d] (%q): download mode %q is not a valid octal string", phase, i, t.Name, t.Download.Mode)
				}
			}
			if t.Download.Sha256 != "" && len(t.Download.Sha256) != 64 {
				return fmt.Errorf("%s[%d] (%q): download sha256 must be 64 hex chars (sha-256 digest), got %d", phase, i, t.Name, len(t.Download.Sha256))
			}
		}
		if t.Apt != nil {
			if len(t.Apt.Name) == 0 && !t.Apt.UpdateCache {
				return fmt.Errorf("%s[%d] (%q): apt requires `name:` (a package or list), or `update_cache: true` for a standalone apt-get update", phase, i, t.Name)
			}
			if t.Apt.State != "" && t.Apt.State != "present" && t.Apt.State != "absent" {
				return fmt.Errorf("%s[%d] (%q): apt state must be 'present' or 'absent', got %q", phase, i, t.Name, t.Apt.State)
			}
		}
		if t.Service != nil {
			if t.Service.Name == "" {
				return fmt.Errorf("%s[%d] (%q): service requires name", phase, i, t.Name)
			}
			switch t.Service.State {
			case "", "started", "stopped", "restarted", "reloaded":
			default:
				return fmt.Errorf("%s[%d] (%q): service state must be one of started/stopped/restarted/reloaded, got %q", phase, i, t.Name, t.Service.State)
			}
			switch t.Service.Provider {
			case "", "systemd", "supervisor":
			default:
				return fmt.Errorf("%s[%d] (%q): service provider must be 'systemd' or 'supervisor', got %q", phase, i, t.Name, t.Service.Provider)
			}
			if t.Service.Provider == "supervisor" && t.Service.Enabled != nil {
				return fmt.Errorf("%s[%d] (%q): 'enabled' only applies to systemd provider", phase, i, t.Name)
			}
		}
		if t.DockerCompose != nil {
			if t.DockerCompose.Dir == "" {
				return fmt.Errorf("%s[%d] (%q): docker_compose requires dir", phase, i, t.Name)
			}
			switch t.DockerCompose.State {
			case "", "up", "down", "restarted", "pulled":
			default:
				return fmt.Errorf("%s[%d] (%q): docker_compose state must be one of up/down/restarted/pulled, got %q", phase, i, t.Name, t.DockerCompose.State)
			}
		}
		if t.DockerBuild != nil {
			if t.DockerBuild.Context == "" {
				return fmt.Errorf("%s[%d] (%q): docker_build requires context", phase, i, t.Name)
			}
			if t.DockerBuild.Tag == "" && len(t.DockerBuild.Tags) == 0 {
				return fmt.Errorf("%s[%d] (%q): docker_build requires at least one tag (tag: or tags:)", phase, i, t.Name)
			}
			if t.DockerBuild.Tag != "" && len(t.DockerBuild.Tags) > 0 {
				return fmt.Errorf("%s[%d] (%q): docker_build: use either 'tag' or 'tags', not both", phase, i, t.Name)
			}
		}
		if t.DockerLogin != nil {
			if t.DockerLogin.Registry == "" {
				return fmt.Errorf("%s[%d] (%q): docker_login requires registry", phase, i, t.Name)
			}
			if t.DockerLogin.CredsFile != "" {
				// creds_file is self-contained; reject the inline fields so the
				// rulebook can't have two competing sources of truth.
				if t.DockerLogin.Username != "" || t.DockerLogin.Password != "" ||
					t.DockerLogin.PasswordFile != "" || t.DockerLogin.PasswordEnv != "" {
					return fmt.Errorf("%s[%d] (%q): docker_login: when creds_file is set, do not also set username/password/password_file/password_env", phase, i, t.Name)
				}
			} else {
				if t.DockerLogin.Username == "" {
					return fmt.Errorf("%s[%d] (%q): docker_login requires creds_file or username", phase, i, t.Name)
				}
				srcs := 0
				if t.DockerLogin.Password != "" {
					srcs++
				}
				if t.DockerLogin.PasswordFile != "" {
					srcs++
				}
				if t.DockerLogin.PasswordEnv != "" {
					srcs++
				}
				if srcs == 0 {
					return fmt.Errorf("%s[%d] (%q): docker_login requires creds_file, or password / password_file / password_env", phase, i, t.Name)
				}
				if srcs > 1 {
					return fmt.Errorf("%s[%d] (%q): docker_login: choose exactly one of password / password_file / password_env", phase, i, t.Name)
				}
			}
			switch t.DockerLogin.Location {
			case "", "both", "local", "remote":
			default:
				return fmt.Errorf("%s[%d] (%q): docker_login location must be one of both/local/remote, got %q", phase, i, t.Name, t.DockerLogin.Location)
			}
		}
		if len(t.WhenChanged) > 0 && (t.Copy != nil || t.Template != nil || t.Mkdir != nil || t.Symlink != nil || t.Remove != nil || t.Chmod != nil || t.Chown != nil || t.Download != nil) {
			return fmt.Errorf("%s[%d] (%q): when_changed is redundant on copy/template/mkdir/symlink/remove/chmod/chown/download (each one stats its path and decides skip-vs-apply on its own)", phase, i, t.Name)
		}
	}
	return nil
}
