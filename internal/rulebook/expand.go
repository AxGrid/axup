package rulebook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// expandUseTasks walks `tasks` and replaces every `use:` node with the body of
// the referenced module (recursively). Module-relative src paths are rewritten
// to absolute paths so the spliced tasks behave the same wherever they end up.
//
// vars merging precedence (highest wins): use.Vars > caller > module defaults.
// callerVars is what the IMPORTING rulebook (and its enclosing modules) had
// available, so deeply-nested modules see everything from above unless they
// shadow it.
func expandUseTasks(tasks []Task, depPaths map[string]string, callerVars map[string]any, visited map[string]bool) ([]Task, error) {
	var out []Task
	for ti := range tasks {
		t := tasks[ti]
		if t.Use == "" {
			if len(t.Vars) > 0 {
				return nil, fmt.Errorf("task %q: `vars:` only applies to `use:` tasks", t.Name)
			}
			out = append(out, t)
			continue
		}

		depName, modulePath, ok := splitUseRef(t.Use)
		if !ok {
			return nil, fmt.Errorf("invalid use path %q (expected <dep>/<module_path>)", t.Use)
		}
		depPath, ok := depPaths[depName]
		if !ok {
			return nil, fmt.Errorf("use %q: dep %q is not declared in deps:", t.Use, depName)
		}
		modulePathAbs := filepath.Join(depPath, modulePath)
		moduleFile := filepath.Join(modulePathAbs, "rulebook.yaml")
		if visited[moduleFile] {
			return nil, fmt.Errorf("circular use: %s", moduleFile)
		}

		module, err := loadModule(moduleFile)
		if err != nil {
			return nil, fmt.Errorf("use %q: %w", t.Use, err)
		}

		// Pre-render the use.vars values against caller vars so the user can
		// write `port: "{{ .mysql_port }}"` in the parent and have the module
		// see a concrete string. Without this step the literal "{{ .x }}"
		// would leak into the module's template expressions.
		preUseVars, err := preRenderVars(t.Vars, callerVars)
		if err != nil {
			return nil, fmt.Errorf("use %q vars: %w", t.Use, err)
		}

		// Merge vars (module defaults < caller < pre-rendered use.vars).
		merged := mergeMaps(module.Vars, callerVars, preUseVars)

		// Mark visited only for the duration of this branch; siblings can
		// reuse the same module legitimately.
		visited[moduleFile] = true
		expanded, err := expandUseTasks(module.Tasks, depPaths, merged, visited)
		delete(visited, moduleFile)
		if err != nil {
			return nil, err
		}

		// Rewrite relative source paths so the host rulebook can read them
		// even though they originated in the module's tree. Then template-
		// expand inline string fields and stamp EffectiveVars so the runner
		// renders template *bodies* with the same merged context.
		for i := range expanded {
			rewriteRelativePaths(&expanded[i], modulePathAbs)
			if err := expandTaskStrings(&expanded[i], merged); err != nil {
				return nil, fmt.Errorf("use %q: %w", t.Use, err)
			}
			if expanded[i].EffectiveVars == nil {
				expanded[i].EffectiveVars = merged
			}
		}
		out = append(out, expanded...)
	}
	return out, nil
}

// preRenderVars walks a use.vars map and template-renders any string values
// against the caller's vars. Non-string values pass through unchanged.
func preRenderVars(useVars, callerVars map[string]any) (map[string]any, error) {
	if len(useVars) == 0 {
		return useVars, nil
	}
	out := make(map[string]any, len(useVars))
	for k, v := range useVars {
		if s, ok := v.(string); ok {
			rendered, err := renderString(s, callerVars)
			if err != nil {
				return nil, fmt.Errorf("var %q: %w", k, err)
			}
			out[k] = rendered
		} else {
			out[k] = v
		}
	}
	return out, nil
}

func splitUseRef(s string) (dep, modulePath string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	i := strings.Index(s, "/")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func mergeMaps(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// rewriteRelativePaths makes file references in a spliced task absolute against
// the module's directory. After this pass, the host rulebook can pretend the
// task was defined in its own tree.
func rewriteRelativePaths(t *Task, moduleDir string) {
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(moduleDir, p)
	}
	if t.Copy != nil {
		t.Copy.Src = abs(t.Copy.Src)
	}
	if t.Template != nil {
		t.Template.Src = abs(t.Template.Src)
	}
	if t.DockerBuild != nil {
		t.DockerBuild.Context = abs(t.DockerBuild.Context)
		t.DockerBuild.Dockerfile = abs(t.DockerBuild.Dockerfile)
	}
	if t.DockerLogin != nil {
		t.DockerLogin.CredsFile = abs(t.DockerLogin.CredsFile)
		t.DockerLogin.PasswordFile = abs(t.DockerLogin.PasswordFile)
	}
}

// loadModule reads a module rulebook (the form with `tasks:` instead of
// `bootstrap`/`deploy`). Module rulebooks may not declare their own deps —
// transitive modules are not supported in the MVP to keep dependency
// resolution simple and predictable.
func loadModule(path string) (*Rulebook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read module %s: %w", path, err)
	}
	var rb Rulebook
	if err := yaml.Unmarshal(data, &rb); err != nil {
		return nil, fmt.Errorf("parse module %s: %w", path, err)
	}
	if rb.Name == "" {
		return nil, fmt.Errorf("module %s: missing required field 'name'", path)
	}
	if len(rb.Tasks) == 0 {
		return nil, fmt.Errorf("module %s: missing 'tasks:' (module rulebooks must use the single-list form)", path)
	}
	if len(rb.Phases) > 0 {
		return nil, fmt.Errorf("module %s: must use 'tasks:' (not phase keys like %v)", path, rb.PhaseNames())
	}
	if len(rb.Deps) > 0 {
		return nil, fmt.Errorf("module %s: transitive deps are not supported in MVP", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	rb.Dir = filepath.Dir(abs)
	return &rb, nil
}
