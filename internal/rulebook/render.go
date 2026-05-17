package rulebook

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// renderString templates a single string field with the rulebook's vars. We
// skip rendering if the field has no `{{` to keep static paths fast and avoid
// surprising errors on plain shell snippets.
func renderString(s string, vars map[string]any) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	tmpl, err := template.New("inline").
		Option("missingkey=error").
		Funcs(sprig.FuncMap()).
		Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse inline template %q: %w", s, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("execute inline template %q: %w", s, err)
	}
	return buf.String(), nil
}

// expandStringFields walks every phase's tasks and replaces template
// expressions in command strings, file paths, and when_changed entries. File
// bodies (copy src, template src) are handled by RenderTemplate / ReadFile at
// task-build time.
func (rb *Rulebook) expandStringFields() error {
	for _, name := range rb.PhaseNames() {
		tasks := rb.Phases[name]
		for i := range tasks {
			if err := expandTaskStrings(&tasks[i], rb.Vars); err != nil {
				return fmt.Errorf("%s[%d]: %w", name, i, err)
			}
		}
	}
	// services: catalog — log paths are templated too so they can reuse
	// rulebook vars (log_dir, …) and inventory host vars.
	for name, svc := range rb.Services {
		for i, p := range svc.Logs {
			r, err := renderString(p, rb.Vars)
			if err != nil {
				return fmt.Errorf("services[%s].logs[%d]: %w", name, i, err)
			}
			svc.Logs[i] = r
		}
		rb.Services[name] = svc
	}
	return nil
}

func expandTaskStrings(t *Task, vars map[string]any) error {
	if t.Command != "" {
		s, err := renderString(t.Command, vars)
		if err != nil {
			return err
		}
		t.Command = s
	}
	if t.Copy != nil {
		// Render both src and dst so a host can choose its own input file via
		// inventory vars: `src: secrets/env.{{ .env }}.yaml`.
		if s, err := renderString(t.Copy.Src, vars); err == nil {
			t.Copy.Src = s
		} else {
			return err
		}
		if s, err := renderString(t.Copy.Dst, vars); err == nil {
			t.Copy.Dst = s
		} else {
			return err
		}
	}
	if t.Template != nil {
		if s, err := renderString(t.Template.Src, vars); err == nil {
			t.Template.Src = s
		} else {
			return err
		}
		if s, err := renderString(t.Template.Dst, vars); err == nil {
			t.Template.Dst = s
		} else {
			return err
		}
	}
	if t.Mkdir != nil {
		if s, err := renderString(t.Mkdir.Path, vars); err == nil {
			t.Mkdir.Path = s
		} else {
			return err
		}
		if s, err := renderString(t.Mkdir.Owner, vars); err == nil {
			t.Mkdir.Owner = s
		} else {
			return err
		}
		if s, err := renderString(t.Mkdir.Group, vars); err == nil {
			t.Mkdir.Group = s
		} else {
			return err
		}
	}
	if t.Symlink != nil {
		if s, err := renderString(t.Symlink.Src, vars); err == nil {
			t.Symlink.Src = s
		} else {
			return err
		}
		if s, err := renderString(t.Symlink.Dst, vars); err == nil {
			t.Symlink.Dst = s
		} else {
			return err
		}
	}
	if t.Remove != nil {
		if s, err := renderString(t.Remove.Path, vars); err == nil {
			t.Remove.Path = s
		} else {
			return err
		}
	}
	if t.User != nil {
		if s, err := renderString(t.User.Name, vars); err == nil {
			t.User.Name = s
		} else {
			return err
		}
		if s, err := renderString(t.User.Home, vars); err == nil {
			t.User.Home = s
		} else {
			return err
		}
		for i, g := range t.User.Groups {
			s, err := renderString(g, vars)
			if err != nil {
				return err
			}
			t.User.Groups[i] = s
		}
	}
	if t.Group != nil {
		if s, err := renderString(t.Group.Name, vars); err == nil {
			t.Group.Name = s
		} else {
			return err
		}
	}
	if t.Chmod != nil {
		if s, err := renderString(t.Chmod.Path, vars); err == nil {
			t.Chmod.Path = s
		} else {
			return err
		}
	}
	if t.Chown != nil {
		if s, err := renderString(t.Chown.Path, vars); err == nil {
			t.Chown.Path = s
		} else {
			return err
		}
		if s, err := renderString(t.Chown.Owner, vars); err == nil {
			t.Chown.Owner = s
		} else {
			return err
		}
		if s, err := renderString(t.Chown.Group, vars); err == nil {
			t.Chown.Group = s
		} else {
			return err
		}
	}
	if t.Download != nil {
		if s, err := renderString(t.Download.URL, vars); err == nil {
			t.Download.URL = s
		} else {
			return err
		}
		if s, err := renderString(t.Download.Dst, vars); err == nil {
			t.Download.Dst = s
		} else {
			return err
		}
		if s, err := renderString(t.Download.Sha256, vars); err == nil {
			t.Download.Sha256 = s
		} else {
			return err
		}
		for k, v := range t.Download.Headers {
			s, err := renderString(v, vars)
			if err != nil {
				return err
			}
			t.Download.Headers[k] = s
		}
	}
	if t.Apt != nil {
		for i, n := range t.Apt.Name {
			s, err := renderString(n, vars)
			if err != nil {
				return err
			}
			t.Apt.Name[i] = s
		}
	}
	if t.Service != nil {
		s, err := renderString(t.Service.Name, vars)
		if err != nil {
			return err
		}
		t.Service.Name = s
	}
	if t.DockerCompose != nil {
		s, err := renderString(t.DockerCompose.Dir, vars)
		if err != nil {
			return err
		}
		t.DockerCompose.Dir = s
	}
	if t.DockerBuild != nil {
		if t.DockerBuild.Tag != "" {
			s, err := renderString(t.DockerBuild.Tag, vars)
			if err != nil {
				return err
			}
			t.DockerBuild.Tag = s
		}
		for i, tag := range t.DockerBuild.Tags {
			s, err := renderString(tag, vars)
			if err != nil {
				return err
			}
			t.DockerBuild.Tags[i] = s
		}
		if t.DockerBuild.Context != "" {
			s, err := renderString(t.DockerBuild.Context, vars)
			if err != nil {
				return err
			}
			t.DockerBuild.Context = s
		}
		if t.DockerBuild.Dockerfile != "" {
			s, err := renderString(t.DockerBuild.Dockerfile, vars)
			if err != nil {
				return err
			}
			t.DockerBuild.Dockerfile = s
		}
		for k, v := range t.DockerBuild.BuildArgs {
			s, err := renderString(v, vars)
			if err != nil {
				return err
			}
			t.DockerBuild.BuildArgs[k] = s
		}
	}
	if t.DockerLogin != nil {
		if s, err := renderString(t.DockerLogin.Registry, vars); err == nil {
			t.DockerLogin.Registry = s
		} else {
			return err
		}
		if s, err := renderString(t.DockerLogin.Username, vars); err == nil {
			t.DockerLogin.Username = s
		} else {
			return err
		}
		// Render the creds-file location so projects can swap per-env keys
		// (`creds_file: secrets/registry.{{ .env }}.yaml`).
		if s, err := renderString(t.DockerLogin.CredsFile, vars); err == nil {
			t.DockerLogin.CredsFile = s
		} else {
			return err
		}
		if s, err := renderString(t.DockerLogin.PasswordFile, vars); err == nil {
			t.DockerLogin.PasswordFile = s
		} else {
			return err
		}
	}
	for i, p := range t.WhenChanged {
		s, err := renderString(p, vars)
		if err != nil {
			return err
		}
		t.WhenChanged[i] = s
	}
	return nil
}

// RenderTemplate reads src (relative to rb.Dir if not absolute), parses it as a
// Go text/template with sprig functions, executes it with `vars`, and returns
// the rendered bytes. Vars are merged from the rulebook's top-level vars and
// any caller-supplied overrides.
func (rb *Rulebook) RenderTemplate(src string, vars map[string]any) ([]byte, error) {
	path := src
	if !filepath.IsAbs(path) {
		path = filepath.Join(rb.Dir, src)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", src, err)
	}
	tmpl, err := template.New(filepath.Base(src)).
		Option("missingkey=error").
		Funcs(sprig.FuncMap()).
		Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", src, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("execute template %s: %w", src, err)
	}
	return buf.Bytes(), nil
}

// ReadFile reads a non-template file (for `copy` tasks) relative to rb.Dir.
func (rb *Rulebook) ReadFile(src string) ([]byte, error) {
	path := src
	if !filepath.IsAbs(path) {
		path = filepath.Join(rb.Dir, src)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", src, err)
	}
	return data, nil
}

// MergeVars combines a base map with overrides. Overrides win.
func MergeVars(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}
