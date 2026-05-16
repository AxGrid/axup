package rulebook

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func Load(path string) (*Rulebook, error) {
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

	if err := validateTasks("bootstrap", rb.Bootstrap); err != nil {
		return nil, err
	}
	if err := validateTasks("deploy", rb.Deploy); err != nil {
		return nil, err
	}
	if err := rb.expandStringFields(); err != nil {
		return nil, err
	}
	return &rb, nil
}

func validateTasks(phase string, tasks []Task) error {
	for i, t := range tasks {
		set := 0
		if t.Command != "" {
			set++
		}
		if t.Copy != nil {
			set++
		}
		if t.Template != nil {
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
		if set == 0 {
			return fmt.Errorf("%s[%d] (%q): no operation set; expected one of command/copy/template/apt/service/docker_compose/docker_install/docker_build/docker_login", phase, i, t.Name)
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
		if t.Apt != nil {
			if len(t.Apt.Name) == 0 {
				return fmt.Errorf("%s[%d] (%q): apt requires at least one package name", phase, i, t.Name)
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
			if t.DockerLogin.Username == "" {
				return fmt.Errorf("%s[%d] (%q): docker_login requires username", phase, i, t.Name)
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
				return fmt.Errorf("%s[%d] (%q): docker_login requires password / password_file / password_env", phase, i, t.Name)
			}
			if srcs > 1 {
				return fmt.Errorf("%s[%d] (%q): docker_login: choose exactly one of password / password_file / password_env", phase, i, t.Name)
			}
			switch t.DockerLogin.Location {
			case "", "both", "local", "remote":
			default:
				return fmt.Errorf("%s[%d] (%q): docker_login location must be one of both/local/remote, got %q", phase, i, t.Name, t.DockerLogin.Location)
			}
		}
		if len(t.WhenChanged) > 0 && (t.Copy != nil || t.Template != nil) {
			return fmt.Errorf("%s[%d] (%q): when_changed is redundant on copy/template (they have their own sha diff)", phase, i, t.Name)
		}
	}
	return nil
}
