package rulebook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// expandCtx threads the state needed to resolve `use:` references through
// recursive expansion. callerDir is the directory of the rulebook doing
// the import — used to resolve `./...` local submodule paths. depPaths is
// the resolved map of git-dep names → local cache directories. phase is
// the current phase name (parent's bootstrap / deploy / …) so implicit-
// phase local refs like `use: ./mysql` know which phase of the submodule
// to splice.
type expandCtx struct {
	callerDir  string
	depPaths   map[string]string
	callerVars map[string]any
	visited    map[string]bool
	phase      string
}

// expandUseTasks walks `tasks` and replaces every `use:` node with the
// body of the referenced module (recursively). Module-relative src paths
// are rewritten to absolute paths so the spliced tasks behave the same
// wherever they end up.
//
// Two flavors of `use:`:
//
//   - git dep: `use: <dep>/<module_path>` — looked up in ctx.depPaths,
//     reads a module-form rulebook (single `tasks:` list).
//   - local submodule: `use: ./<path>` or `use: ./<path>/<phase>` —
//     resolved relative to ctx.callerDir, reads a phased rulebook
//     (the submodule has its own `bootstrap:`, `deploy:`, …). The
//     selected phase is spliced. If only `./path` is given (no phase
//     suffix), the parent's current phase name is used implicitly.
//
// vars merging precedence (highest wins): use.Vars > caller > module
// defaults. callerVars is what the IMPORTING rulebook had available, so
// deeply-nested modules see everything from above unless they shadow it.
func expandUseTasks(tasks []Task, ctx *expandCtx) ([]Task, error) {
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

		var (
			moduleDir   string
			moduleTasks []Task
			moduleVars  map[string]any
			visitKey    string
		)

		if isLocalUseRef(t.Use) {
			// Local submodule: resolve relative to caller's rulebook dir,
			// then either pick the named phase or fall back to the parent's
			// current phase.
			subDir, subPhase, err := resolveLocalUseRef(t.Use, ctx.callerDir, ctx.phase)
			if err != nil {
				return nil, fmt.Errorf("use %q: %w", t.Use, err)
			}
			sub, err := loadLocalSubmodule(filepath.Join(subDir, "rulebook.yaml"))
			if err != nil {
				return nil, fmt.Errorf("use %q: %w", t.Use, err)
			}
			phaseTasks, ok := sub.Phases[subPhase]
			if !ok {
				return nil, fmt.Errorf("use %q: submodule %q at %s has no phase %q (available: %v)",
					t.Use, sub.Name, subDir, subPhase, sub.PhaseNames())
			}
			moduleDir = subDir
			moduleTasks = phaseTasks
			moduleVars = sub.Vars
			visitKey = subDir + "#" + subPhase
		} else {
			depName, modulePath, ok := splitUseRef(t.Use)
			if !ok {
				return nil, fmt.Errorf("invalid use path %q (expected <dep>/<module_path> or ./<local_path>[/<phase>])", t.Use)
			}
			depPath, ok := ctx.depPaths[depName]
			if !ok {
				return nil, fmt.Errorf("use %q: dep %q is not declared in deps:", t.Use, depName)
			}
			moduleDir = filepath.Join(depPath, modulePath)
			moduleFile := filepath.Join(moduleDir, "rulebook.yaml")
			module, err := loadModule(moduleFile)
			if err != nil {
				return nil, fmt.Errorf("use %q: %w", t.Use, err)
			}
			moduleTasks = module.Tasks
			moduleVars = module.Vars
			visitKey = moduleFile + "#tasks"
		}

		if ctx.visited[visitKey] {
			return nil, fmt.Errorf("circular use: %s", visitKey)
		}

		// Pre-render the use.vars values against caller vars so the user can
		// write `port: "{{ .mysql_port }}"` in the parent and have the module
		// see a concrete string. Without this step the literal "{{ .x }}"
		// would leak into the module's template expressions.
		preUseVars, err := preRenderVars(t.Vars, ctx.callerVars)
		if err != nil {
			return nil, fmt.Errorf("use %q vars: %w", t.Use, err)
		}

		// Merge vars (module defaults < caller < pre-rendered use.vars).
		merged := mergeMaps(moduleVars, ctx.callerVars, preUseVars)

		// Mark visited only for the duration of this branch; siblings can
		// reuse the same module legitimately.
		ctx.visited[visitKey] = true
		childCtx := &expandCtx{
			callerDir:  moduleDir,
			depPaths:   ctx.depPaths,
			callerVars: merged,
			visited:    ctx.visited,
			// For local submodules, nested implicit-phase refs (e.g.
			// `use: ./helper` inside mysql's bootstrap) should continue to
			// resolve against the SUBMODULE's phase, not the original
			// parent's. For git modules the value is unused (their refs
			// are dep-keyed, not phase-keyed).
			phase: extractPhase(visitKey),
		}
		expanded, err := expandUseTasks(moduleTasks, childCtx)
		delete(ctx.visited, visitKey)
		if err != nil {
			return nil, err
		}

		// Rewrite relative source paths so the host rulebook can read them
		// even though they originated in the module's tree. Then template-
		// expand inline string fields and stamp EffectiveVars so the runner
		// renders template *bodies* with the same merged context.
		for i := range expanded {
			rewriteRelativePaths(&expanded[i], moduleDir)
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

// isLocalUseRef reports whether `s` is a filesystem path (`./`, `../`, or
// absolute) rather than a `<dep>/<module>` reference into deps[].
func isLocalUseRef(s string) bool {
	return strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || strings.HasPrefix(s, "/")
}

// resolveLocalUseRef interprets a local `use:` reference and returns the
// absolute submodule directory plus the phase name to splice.
//
// Disambiguation: try the whole ref as a directory first (rulebook.yaml
// must exist in it). If that succeeds, the phase is implicit (= parent's
// current phase). Otherwise split off the last segment as an explicit
// phase name and look for rulebook.yaml in the remaining prefix.
func resolveLocalUseRef(ref, callerDir, parentPhase string) (subDir, subPhase string, err error) {
	cleanRef := strings.TrimRight(ref, "/")
	if cleanRef == "" {
		return "", "", fmt.Errorf("empty local ref")
	}
	full := joinIfRel(cleanRef, callerDir)
	if hasRulebook(full) {
		if parentPhase == "" {
			return "", "", fmt.Errorf("implicit-phase form `use: %s` needs to be invoked from inside a phase", ref)
		}
		return full, parentPhase, nil
	}
	idx := strings.LastIndex(cleanRef, "/")
	if idx <= 0 {
		return "", "", fmt.Errorf("no rulebook.yaml at %s and ref has no phase suffix to fall back to", full)
	}
	head, tail := cleanRef[:idx], cleanRef[idx+1:]
	headAbs := joinIfRel(head, callerDir)
	if hasRulebook(headAbs) {
		return headAbs, tail, nil
	}
	return "", "", fmt.Errorf("no rulebook.yaml at %s (also tried %s)", full, headAbs)
}

func joinIfRel(p, base string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func hasRulebook(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "rulebook.yaml"))
	return err == nil
}

// extractPhase pulls the phase suffix back out of a visitKey ("<dir>#<phase>").
// Used to seed the recursive ctx so nested implicit-phase refs resolve
// against the right phase. For git-module visitKeys ("<file>#tasks") this
// returns "tasks", which is harmless because git modules never use the
// phase field of expandCtx (they're resolved by dep name).
func extractPhase(visitKey string) string {
	if i := strings.LastIndex(visitKey, "#"); i >= 0 {
		return visitKey[i+1:]
	}
	return ""
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

// loadLocalSubmodule reads a phased local-submodule rulebook (bootstrap:/
// deploy:/custom phases). Unlike loadModule, this is for in-repo
// subdirectories — no git/version semantics. Parent's expandUseTasks picks
// one of the phases to splice; the others are inert until referenced.
//
// Restrictions for MVP:
//   - submodule must be phased (no `tasks:` module-form mix-in)
//   - submodule's own `deps:` rejected (transitive deps out of scope)
//   - submodule's `services:` / `secrets:` / `history:` are silently
//     ignored — only `tasks:` (per phase) and `vars:` are imported. The
//     parent rulebook owns those concerns centrally.
func loadLocalSubmodule(path string) (*Rulebook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read submodule %s: %w", path, err)
	}
	var rb Rulebook
	if err := yaml.Unmarshal(data, &rb); err != nil {
		return nil, fmt.Errorf("parse submodule %s: %w", path, err)
	}
	if rb.Name == "" {
		return nil, fmt.Errorf("submodule %s: missing required field 'name'", path)
	}
	if len(rb.Tasks) > 0 {
		return nil, fmt.Errorf("submodule %s: local submodules must be phased (bootstrap:/deploy:/…), not module-form `tasks:`. For module-form reuse, declare it as a git dep.", path)
	}
	if len(rb.Phases) == 0 {
		return nil, fmt.Errorf("submodule %s: no phases declared", path)
	}
	if len(rb.Deps) > 0 {
		return nil, fmt.Errorf("submodule %s: transitive deps are not supported (submodule cannot declare its own deps:)", path)
	}
	if err := validatePhaseNames(rb.Phases); err != nil {
		return nil, fmt.Errorf("submodule %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	rb.Dir = filepath.Dir(abs)
	return &rb, nil
}
