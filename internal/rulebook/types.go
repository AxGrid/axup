package rulebook

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type Rulebook struct {
	Name      string         `yaml:"name"`
	Vars      map[string]any `yaml:"vars,omitempty"`
	Bootstrap []Task         `yaml:"bootstrap,omitempty"`
	Deploy    []Task         `yaml:"deploy,omitempty"`

	// Dir is the directory containing the rulebook.yaml, used to resolve
	// relative src paths in copy/template tasks. Set by Load, not parsed.
	Dir string `yaml:"-"`
}

// Task is exactly one of: command, copy, template, apt, service,
// docker_compose, docker_install.
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
	WhenChanged   []string           `yaml:"when_changed,omitempty"`
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
// `docker_compose` can pull private images). Exactly one password source must
// be set.
type DockerLoginSpec struct {
	Registry     string `yaml:"registry"`
	Username     string `yaml:"username"`
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
	default:
		return ""
	}
}
