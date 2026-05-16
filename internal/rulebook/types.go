package rulebook

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

type Rulebook struct {
	Name     string             `yaml:"name"`
	Vars     map[string]any     `yaml:"vars,omitempty"`
	Deps     []DepSpec          `yaml:"deps,omitempty"`     // top-level only
	Tasks    []Task             `yaml:"tasks,omitempty"`    // module form: a single reusable list
	Secrets  *SecretsSpec       `yaml:"secrets,omitempty"`  // declarative encrypted-files list
	Services map[string]Service `yaml:"services,omitempty"` // catalog used by `deploy logs <name>`

	// Phases captures every top-level key that isn't one of the reserved
	// fields above — `bootstrap:`, `deploy:`, `deploy_crash:`, `migrate:`,
	// any user-defined name (regex ^[a-z][a-z0-9_-]*$). The CLI dispatches
	// via `deploy bootstrap`, `deploy deploy`, or `deploy run <phase>`.
	Phases map[string][]Task `yaml:",inline"`

	// Dir is the directory containing the rulebook.yaml, used to resolve
	// relative src paths in copy/template tasks. Set by Load, not parsed.
	Dir string `yaml:"-"`
}

// Service is a catalog entry consumed by `deploy logs <name>`. It does
// NOT manage the running process — that's still `task.Service` (systemd
// /supervisor). This is purely informational: "where are this service's
// logs on the remote".
type Service struct {
	Logs StringOrList `yaml:"logs"` // one path or a list; templated through rb.Vars
}

// reservedPhaseNames are CLI-internal phase strings the runner uses for
// special-case dispatch — users can't define a rulebook phase with these
// names because it'd collide with the dispatch logic.
var reservedPhaseNames = map[string]struct{}{
	"status":   {}, // `deploy status` walks state.json, not a real phase
	"tasks":    {}, // module-form key; lives in its own struct field
	"services": {}, // catalog block; lives in its own struct field
	"logs":     {}, // `deploy logs` subcommand
}

// Phase returns the task list for `name`, or nil if no such phase exists.
// Always prefer this helper over reading r.Phases directly so a future move
// of a phase to its own struct field stays a one-line change.
func (r *Rulebook) Phase(name string) []Task {
	if r.Phases == nil {
		return nil
	}
	return r.Phases[name]
}

// PhaseNames returns the declared phase names, sorted, so error messages
// and `deploy status` / scaffold output are stable.
func (r *Rulebook) PhaseNames() []string {
	out := make([]string, 0, len(r.Phases))
	for k := range r.Phases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SecretsSpec declares which files in the project are managed by `deploy
// secrets` (encrypt in place against the recipients listed in recipients.txt).
// Used by `deploy secrets encrypt` (no args) to iterate the list and by
// `deploy secrets status` to surface plaintext-leftover problems.
//
// The runtime decrypt path (when a task reads a creds_file / password_file) is
// independent — it auto-detects ciphertext by content, regardless of whether
// the file is declared here. So the block is opt-in convenience, not enforcement.
type SecretsSpec struct {
	RecipientsFile string   `yaml:"recipients_file,omitempty"` // default: recipients.txt next to rulebook
	Files          []string `yaml:"files,omitempty"`           // paths relative to rulebook dir
}

// DepSpec declares a git repository whose modules are imported via `use:`.
type DepSpec struct {
	Name    string `yaml:"name"`    // local handle used in `use: <name>/<path>`
	Git     string `yaml:"git"`     // "github.com/foo/bar", "git@…", or "https://…"
	Version string `yaml:"version"` // tag, branch, or commit sha
}

// Task is exactly one of: command, copy, template, apt, service,
// docker_compose, docker_install, docker_build, docker_login, or use.
// `use` is special — it's a reference to a module that gets inlined at parse
// time and disappears from the final task list.
type Task struct {
	Name          string             `yaml:"name,omitempty"`
	Command       string             `yaml:"command,omitempty"`
	Copy          *CopySpec          `yaml:"copy,omitempty"`
	Template      *TemplateSpec      `yaml:"template,omitempty"`
	Apt           *AptSpec           `yaml:"apt,omitempty"`
	Service       *ServiceSpec       `yaml:"service,omitempty"`
	DockerCompose *DockerComposeSpec `yaml:"docker_compose,omitempty"`
	DockerInstall *DockerInstallSpec `yaml:"docker_install,omitempty"`
	DockerBuild   *DockerBuildSpec   `yaml:"docker_build,omitempty"`
	DockerLogin   *DockerLoginSpec   `yaml:"docker_login,omitempty"`
	Use           string             `yaml:"use,omitempty"`  // "<dep>/<module_path>"
	Vars          map[string]any     `yaml:"vars,omitempty"` // passed to the imported module
	WhenChanged   []string           `yaml:"when_changed,omitempty"`

	// EffectiveVars carries the merged var context for tasks that came from a
	// use: expansion (module defaults < caller < pre-rendered use.vars). It is
	// nil for top-level tasks, in which case rb.Vars is used. Not parsed from
	// YAML — set by the expander.
	EffectiveVars map[string]any `yaml:"-"`
}

type CopySpec struct {
	Src  string `yaml:"src"`
	Dst  string `yaml:"dst"`
	Mode string `yaml:"mode,omitempty"`
}

type TemplateSpec struct {
	Src  string `yaml:"src"`
	Dst  string `yaml:"dst"`
	Mode string `yaml:"mode,omitempty"`
}

type AptSpec struct {
	Name        StringOrList `yaml:"name"`
	State       string       `yaml:"state,omitempty"`        // present (default), absent
	UpdateCache bool         `yaml:"update_cache,omitempty"` // apt-get update first
}

type ServiceSpec struct {
	Name     string `yaml:"name"`
	State    string `yaml:"state,omitempty"`    // started, stopped, restarted, reloaded
	Enabled  *bool  `yaml:"enabled,omitempty"`  // systemd only
	Provider string `yaml:"provider,omitempty"` // systemd (default), supervisor
}

type DockerComposeSpec struct {
	Dir   string `yaml:"dir"`
	State string `yaml:"state,omitempty"` // up (default), down, restarted, pulled
	Pull  bool   `yaml:"pull,omitempty"`  // docker compose pull before up
	Wait  bool   `yaml:"wait,omitempty"`  // append --wait to `docker compose up` (blocks until healthchecks pass)
}

// DockerInstallSpec triggers the official get.docker.com installer. Skipped if
// `docker --version` already succeeds. Empty for now — reserved for future
// channel/version knobs without breaking YAML compatibility.
type DockerInstallSpec struct{}

// DockerBuildSpec drives `docker buildx build` on the CLI host (not the
// remote). Common usage: build, tag with git_sha, push to a registry, then a
// downstream docker_compose task on the remote pulls by that tag.
type DockerBuildSpec struct {
	Context    string            `yaml:"context"`              // build context dir
	Dockerfile string            `yaml:"dockerfile,omitempty"` // default "Dockerfile" relative to context
	Tag        string            `yaml:"tag,omitempty"`        // single tag
	Tags       []string          `yaml:"tags,omitempty"`       // multiple tags (or use Tag)
	Push       bool              `yaml:"push,omitempty"`
	Platform   string            `yaml:"platform,omitempty"` // default "linux/amd64"
	BuildArgs  map[string]string `yaml:"build_args,omitempty"`
}

// DockerLoginSpec runs `docker login` against a registry, by default both on
// the CLI host (so local `docker_build --push` works) and on the remote (so
// `docker_compose` can pull private images).
//
// Credentials come from one of two sources:
//
//   - creds_file: a per-project YAML file containing {username, password}.
//     Path is relative to the rulebook dir if not absolute. This is the
//     preferred form because it keeps creds out of the rulebook and out of
//     git (secrets/ is gitignored), and the file format is forward-compatible
//     with planned age/sops encryption — the CLI will detect ciphertext and
//     decrypt transparently in a later phase.
//
//   - inline username + (password | password_file | password_env): legacy
//     form. Useful when you already have a password in an env var (e.g. CI).
type DockerLoginSpec struct {
	Registry     string `yaml:"registry"`
	CredsFile    string `yaml:"creds_file,omitempty"`    // path to YAML with {username, password}
	Username     string `yaml:"username,omitempty"`      // unused when creds_file is set
	Password     string `yaml:"password,omitempty"`      // inline (least preferred)
	PasswordFile string `yaml:"password_file,omitempty"` // path relative to rulebook
	PasswordEnv  string `yaml:"password_env,omitempty"`  // read $VAR at parse time
	Location     string `yaml:"location,omitempty"`      // both (default) | local | remote
}

// StringOrList lets a YAML field accept either "foo" or [foo, bar, …].
type StringOrList []string

func (s *StringOrList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*s = []string{node.Value}
		return nil
	case yaml.SequenceNode:
		var arr []string
		if err := node.Decode(&arr); err != nil {
			return err
		}
		*s = arr
		return nil
	default:
		return fmt.Errorf("expected string or list at line %d", node.Line)
	}
}

// Kind returns the discriminator for this task.
func (t Task) Kind() string {
	switch {
	case t.Command != "":
		return "command"
	case t.Copy != nil:
		return "copy"
	case t.Template != nil:
		return "template"
	case t.Apt != nil:
		return "apt"
	case t.Service != nil:
		return "service"
	case t.DockerCompose != nil:
		return "docker_compose"
	case t.DockerInstall != nil:
		return "docker_install"
	case t.DockerBuild != nil:
		return "docker_build"
	case t.DockerLogin != nil:
		return "docker_login"
	case t.Use != "":
		return "use"
	default:
		return ""
	}
}
