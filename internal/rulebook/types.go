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
	Services map[string]Service `yaml:"services,omitempty"` // catalog used by `axup logs <name>`
	History  int                `yaml:"history,omitempty"`  // how many previous versions per copy/template file to keep on the remote for `axup rollback` (0 = disabled, default; 3 is the typical opt-in)

	// Phases captures every top-level key that isn't one of the reserved
	// fields above — `bootstrap:`, `deploy:`, `deploy_crash:`, `migrate:`,
	// any user-defined name (regex ^[a-z][a-z0-9_-]*$). The CLI dispatches
	// via `axup bootstrap`, `axup deploy`, or `axup run <phase>`.
	Phases map[string][]Task `yaml:",inline"`

	// Dir is the directory containing the rulebook.yaml, used to resolve
	// relative src paths in copy/template tasks. Set by Load, not parsed.
	Dir string `yaml:"-"`
}

// Service is a catalog entry consumed by `axup logs <name>`. It does
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
	"status":   {}, // `axup status` walks state.json, not a real phase
	"tasks":    {}, // module-form key; lives in its own struct field
	"services": {}, // catalog block; lives in its own struct field
	"logs":     {}, // `axup logs` subcommand
	"history":  {}, // top-level int field (depth of per-file rollback chain)
	"rollback": {}, // `axup rollback` subcommand
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
// and `axup status` / scaffold output are stable.
func (r *Rulebook) PhaseNames() []string {
	out := make([]string, 0, len(r.Phases))
	for k := range r.Phases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SecretsSpec declares which files in the project are managed by `axup
// secrets` (encrypt in place against the recipients listed in recipients.txt).
// Used by `axup secrets encrypt` (no args) to iterate the list and by
// `axup secrets status` to surface plaintext-leftover problems.
//
// The runtime decrypt path (when a task reads a creds_file / password_file, or
// when inventory.yaml is loaded) is independent — it auto-detects ciphertext by
// content, regardless of whether the file is declared here. So the block is
// opt-in convenience, not enforcement.
type SecretsSpec struct {
	RecipientsFile string       `yaml:"recipients_file,omitempty"` // default: recipients.txt next to rulebook
	Files          []SecretFile `yaml:"files,omitempty"`           // paths relative to rulebook dir
}

// SecretFile is one entry in `secrets.files`. It accepts two YAML shapes:
//
//	files:
//	  - secrets/db.pw                           # simple string → default recipients
//	  - path: inventory.stage.yaml              # mapping → per-file recipients override
//	    recipients: recipients.stage.txt
//	  - path: inventory.prod.yaml               # …or force a specific encryption mode
//	    recipients: recipients.prod.txt
//	    format: age                             # whole-file age (default for structured
//	                                            #   files is "sops"; "age" forces the
//	                                            #   whole-file mode so ssh-* recipients
//	                                            #   keep working on .yaml/.json/.env/…)
//	  - path: inventory.prod.enc.yaml           # pair mode: `path` is the encrypted
//	    decrypted_to: inventory.prod.yaml       #   file committed to git, `decrypted_to`
//	    recipients: recipients.prod.txt         #   is the plaintext working copy that
//	    format: age                             #   `axup secrets unseal` materialises and
//	                                            #   `axup secrets seal` re-encrypts back.
//	                                            #   axup auto-adds decrypted_to to the
//	                                            #   nearest .gitignore.
//
// Per-file `recipients` lets one rulebook manage several recipient groups —
// e.g. one age-key set scoped to stage hosts, another scoped to prod.
//
// `format` is one of:
//   - ""     (default): pick by extension — yaml/yml/json/ini/env/toml → sops
//     (per-leaf, structure-preserving) and anything else → age (whole-file).
//   - "age": force whole-file age regardless of extension. Use this when
//     recipients are ssh-* keys (sops only takes age1… recipients).
//   - "sops": force sops (only valid for the structured extensions listed
//     above; recipients must be age1…).
//
// `decrypted_to`, when set, switches the entry to "pair mode" — `path` is the
// committed encrypted file, `decrypted_to` is the plaintext working file. Use
// `seal`/`unseal` to move bytes between them. `encrypt`/`decrypt` skip pair
// entries (with a hint). Entries without `decrypted_to` keep the in-place
// behaviour: `path` is encrypted in-place.
type SecretFile struct {
	Path         string `yaml:"path"`                    // path relative to rulebook dir
	Recipients   string `yaml:"recipients,omitempty"`    // optional override of SecretsSpec.RecipientsFile
	Format       string `yaml:"format,omitempty"`        // "age", "sops", or "" (auto by extension)
	DecryptedTo  string `yaml:"decrypted_to,omitempty"`  // optional: plaintext target for `unseal`; presence switches the entry to pair mode
}

// IsPair reports whether this entry uses the pair (encrypted-at-rest +
// plaintext-on-disk) model. Pair entries are handled by `seal` / `unseal`;
// non-pair entries by `encrypt` / `decrypt` in place.
func (s SecretFile) IsPair() bool { return s.DecryptedTo != "" }

// UnmarshalYAML accepts either a bare string or a {path, recipients} mapping.
// Keeps `files: [a.yaml, b.yaml]` working while allowing the longer form for
// per-file recipient overrides.
func (s *SecretFile) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		s.Path = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("secrets.files entry: expected string or mapping, got %s", nodeKind(node.Kind))
	}
	// Use a sibling struct to avoid recursion into UnmarshalYAML.
	type alias SecretFile
	var a alias
	if err := node.Decode(&a); err != nil {
		return err
	}
	if a.Path == "" {
		return fmt.Errorf("secrets.files entry: 'path' is required")
	}
	*s = SecretFile(a)
	return nil
}

func nodeKind(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	}
	return "unknown"
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
	Command       *CommandSpec       `yaml:"command,omitempty"`
	Copy          *CopySpec          `yaml:"copy,omitempty"`
	Template      *TemplateSpec      `yaml:"template,omitempty"`
	Mkdir         *MkdirSpec         `yaml:"mkdir,omitempty"`
	Symlink       *SymlinkSpec       `yaml:"symlink,omitempty"`
	Remove        *RemoveSpec        `yaml:"remove,omitempty"`
	User          *UserSpec          `yaml:"user,omitempty"`
	Group         *GroupSpec         `yaml:"group,omitempty"`
	Chmod         *ChmodSpec         `yaml:"chmod,omitempty"`
	Chown         *ChownSpec         `yaml:"chown,omitempty"`
	Download      *DownloadSpec      `yaml:"download,omitempty"`
	Apt           *AptSpec           `yaml:"apt,omitempty"`
	Service       *ServiceSpec       `yaml:"service,omitempty"`
	DockerCompose *DockerComposeSpec `yaml:"docker_compose,omitempty"`
	DockerInstall *DockerInstallSpec `yaml:"docker_install,omitempty"`
	DockerBuild   *DockerBuildSpec   `yaml:"docker_build,omitempty"`
	DockerLogin   *DockerLoginSpec   `yaml:"docker_login,omitempty"`
	MysqlDatabase *MysqlDatabaseSpec `yaml:"mysql_database,omitempty"`
	PgDatabase    *PgDatabaseSpec    `yaml:"pg_database,omitempty"`
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

// CommandSpec describes a shell command to run on the remote, with optional
// gating predicates and an ignore_errors escape hatch. Accepts two YAML
// surfaces:
//
//	command: echo hi                       # shorthand: just the command string
//	command:                               # full form: gate + ignore_errors
//	  run: swapon --add /swapfile
//	  unless: swapon --show | grep -q .
//	  ignore_errors: true
//
// `when:` runs the body only when the predicate exits 0; `unless:` is the
// inverse (run only when the predicate exits non-0). They are mutually
// exclusive. `ignore_errors:` flips a non-zero exit from `error` to `changed`
// so the rest of the phase keeps going — handy for "stop kiosk || true"
// patterns.
type CommandSpec struct {
	Run          string `yaml:"run"`
	When         string `yaml:"when,omitempty"`
	Unless       string `yaml:"unless,omitempty"`
	IgnoreErrors bool   `yaml:"ignore_errors,omitempty"`
}

// UnmarshalYAML accepts either a bare command string ("command: echo hi") or
// the full CommandSpec object form.
func (c *CommandSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		c.Run = node.Value
		return nil
	case yaml.MappingNode:
		type raw CommandSpec
		var r raw
		if err := node.Decode(&r); err != nil {
			return err
		}
		*c = CommandSpec(r)
		return nil
	default:
		return fmt.Errorf("command: expected string or mapping at line %d", node.Line)
	}
}

// MkdirSpec creates a directory on the remote with mode/owner/group. Shorthand
// `mkdir: /path` is sugar for `mkdir: { path: /path }`. Always recursive
// (mkdir -p semantics) — parent dirs are created automatically.
//
// Idempotency: stat the path. Already-a-dir → diff mode/owner/group and chmod/
// chown as needed. Already-a-regular-file → error (we never auto-rm). Absent
// → MkdirAll + chmod + optional chown.
type MkdirSpec struct {
	Path  string `yaml:"path"`
	Mode  string `yaml:"mode,omitempty"`  // octal string, e.g. "0755"; empty = 0755
	Owner string `yaml:"owner,omitempty"` // optional username; resolved on the remote via /etc/passwd
	Group string `yaml:"group,omitempty"` // optional group name; resolved via /etc/group
}

// UnmarshalYAML accepts either a bare path string ("mkdir: /opt/foo") or a
// mapping with explicit fields. Mirrors SecretFile's two-shape pattern.
func (m *MkdirSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		m.Path = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("mkdir: expected string path or mapping, got %s", nodeKind(node.Kind))
	}
	type alias MkdirSpec
	var a alias
	if err := node.Decode(&a); err != nil {
		return err
	}
	*m = MkdirSpec(a)
	return nil
}

// SymlinkSpec manages a symbolic link. No shorthand — two required fields.
// `force: true` (default) replaces an existing symlink whose target differs
// AND replaces an existing regular file at dst (the typical `ln -sfn` use
// case). `force: false` is conservative: keeps an existing real file at dst,
// reports an error to surface the conflict.
//
// Idempotency: when dst is already a symlink AND os.Readlink(dst) == src,
// the task is skipped.
type SymlinkSpec struct {
	Src   string `yaml:"src"`             // target the link points TO
	Dst   string `yaml:"dst"`             // the symlink path itself
	Force *bool  `yaml:"force,omitempty"` // default true; set explicitly to false to refuse to overwrite a regular file at dst
}

// RemoveSpec deletes a path. Shorthand `remove: /path` is sugar for
// `remove: { path: /path }`. `recursive: true` is required to remove a
// non-empty directory (rm -rf semantics) — default false is safer and
// matches `rmdir`/`unlink` behavior.
//
// Idempotency: absent → skipped. Always operates on the symlink itself,
// never follows it.
type RemoveSpec struct {
	Path      string `yaml:"path"`
	Recursive bool   `yaml:"recursive,omitempty"` // default false; true = rm -rf
}

// UserSpec creates or deletes a system user on the remote. MVP scope: it does
// NOT reconcile attributes of an existing user (shell, home, uid, primary
// group) — present-state is "exists with the right name", absent-state is
// "doesn't exist". Supplementary groups are the one exception: reconciled
// via `usermod -aG <group>` each run, so a new group added to the rulebook
// gets attached without recreating the user.
//
// Always passes `-r` (system user). For interactive users, drop down to
// `command:` — that's a less common need for ops tooling.
//
// Shorthand `user: app` is sugar for `user: { name: app }`.
type UserSpec struct {
	Name       string   `yaml:"name"`
	Shell      string   `yaml:"shell,omitempty"`       // default /usr/sbin/nologin
	Home       string   `yaml:"home,omitempty"`        // when set, passed as --home-dir
	CreateHome bool     `yaml:"create_home,omitempty"` // pair with home: to actually mkdir it
	Groups     []string `yaml:"groups,omitempty"`      // supplementary groups
	State      string   `yaml:"state,omitempty"`       // present (default) | absent
}

func (u *UserSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		u.Name = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("user: expected string name or mapping, got %s", nodeKind(node.Kind))
	}
	type alias UserSpec
	var a alias
	if err := node.Decode(&a); err != nil {
		return err
	}
	*u = UserSpec(a)
	return nil
}

// GroupSpec creates or deletes a system group on the remote. Mirrors
// UserSpec's MVP scope — exists-or-not, no attribute reconciliation
// (gid pinning has the same destructive-failure modes as uid pinning
// for users). Always passes `-r` (system group) on create.
//
// Shorthand `group: docker` is sugar for `group: { name: docker }`.
type GroupSpec struct {
	Name  string `yaml:"name"`
	State string `yaml:"state,omitempty"` // present (default) | absent
}

func (g *GroupSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		g.Name = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("group: expected string name or mapping, got %s", nodeKind(node.Kind))
	}
	type alias GroupSpec
	var a alias
	if err := node.Decode(&a); err != nil {
		return err
	}
	*g = GroupSpec(a)
	return nil
}

// ChmodSpec sets POSIX permissions on an existing path. Idempotent by
// stat — current mode matches → skipped, else chmod → changed.
//
// No recursive support in MVP: a single `mode:` knob can't sensibly
// apply to both dirs and files in a tree (dirs typically want `+x`,
// files don't). For tree-wide perm management use a `command:` task
// (`find -type d -exec chmod 0755 + ; find -type f -exec chmod 0644 +`).
type ChmodSpec struct {
	Path string `yaml:"path"`
	Mode string `yaml:"mode"`
}

// ChownSpec sets owner / group on an existing path. At least one of
// owner / group must be set. Recursive flag walks the tree with `chown
// -R` semantics — idempotency-wise we only check the TOP path's
// current ownership (assumption: tree state matches top, which holds
// when previous chown runs all went through this task). For surgical
// per-file checks, run the task non-recursively at the target file.
type ChownSpec struct {
	Path      string `yaml:"path"`
	Owner     string `yaml:"owner,omitempty"`
	Group     string `yaml:"group,omitempty"`
	Recursive bool   `yaml:"recursive,omitempty"` // default false
}

// DownloadSpec fetches a URL to a path on the remote. Idempotency:
//   - dst absent → download → changed
//   - dst present + sha256 set + matches → skipped
//   - dst present + sha256 set + mismatch → re-download → changed
//   - dst present + no sha256 → skipped (assume OK; doc recommends pinning sha)
//
// Headers map enables Authorization / custom headers for private endpoints.
// Writes atomically (tmp + rename) so a half-downloaded file never appears
// at dst. Default timeout 10 min; default mode 0644.
type DownloadSpec struct {
	URL     string            `yaml:"url"`
	Dst     string            `yaml:"dst"`
	Mode    string            `yaml:"mode,omitempty"`    // octal string; default 0644
	Sha256  string            `yaml:"sha256,omitempty"`  // optional hex digest for verification
	Headers map[string]string `yaml:"headers,omitempty"` // optional HTTP headers
}

func (r *RemoveSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		r.Path = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("remove: expected string path or mapping, got %s", nodeKind(node.Kind))
	}
	type alias RemoveSpec
	var a alias
	if err := node.Decode(&a); err != nil {
		return err
	}
	*r = RemoveSpec(a)
	return nil
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

// MysqlDatabaseSpec ensures a MySQL database exists, and (when user+password
// are set) an application user with full privileges on that database. The
// agent shells out to `mysql` on the remote with credentials passed via
// MYSQL_PWD so they don't appear in `ps`. Connection params are mandatory —
// no implicit socket / peer auth.
//
// Idempotency: existence is checked against information_schema.SCHEMATA and
// mysql.user before any CREATE runs, so a re-run on a converged DB reports
// `skipped`. Charset/collation drift on an already-existing DB is NOT
// reconciled — this task only creates, never alters.
type MysqlDatabaseSpec struct {
	Name          string `yaml:"name"`
	Host          string `yaml:"host"`
	Port          int    `yaml:"port,omitempty"` // default 3306
	AdminUser     string `yaml:"admin_user"`
	AdminPassword string `yaml:"admin_password"`
	Charset       string `yaml:"charset,omitempty"`   // default utf8mb4
	Collation     string `yaml:"collation,omitempty"` // default utf8mb4_0900_ai_ci
	User          string `yaml:"user,omitempty"`      // optional app user
	Password      string `yaml:"password,omitempty"`  // required when user: is set
	UserHost      string `yaml:"user_host,omitempty"` // grant scope; default '%'
}

// PgDatabaseSpec ensures a PostgreSQL database exists, and (when user+password
// are set) a role with LOGIN + full privileges on that database. The agent
// shells out to `psql` on the remote with credentials passed via PGPASSWORD.
// Connection params are mandatory — no implicit peer/ident auth.
//
// Idempotency: existence is checked against pg_database and pg_roles before
// CREATE runs. Encoding/owner drift on an already-existing DB is NOT
// reconciled — create-only.
type PgDatabaseSpec struct {
	Name          string `yaml:"name"`
	Host          string `yaml:"host"`
	Port          int    `yaml:"port,omitempty"` // default 5432
	AdminUser     string `yaml:"admin_user"`
	AdminPassword string `yaml:"admin_password"`
	Encoding      string `yaml:"encoding,omitempty"` // default UTF8
	Owner         string `yaml:"owner,omitempty"`    // defaults to user: when set, else admin_user
	User          string `yaml:"user,omitempty"`     // optional app role
	Password      string `yaml:"password,omitempty"` // required when user: is set
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
	case t.Command != nil:
		return "command"
	case t.Copy != nil:
		return "copy"
	case t.Template != nil:
		return "template"
	case t.Mkdir != nil:
		return "mkdir"
	case t.Symlink != nil:
		return "symlink"
	case t.Remove != nil:
		return "remove"
	case t.User != nil:
		return "user"
	case t.Group != nil:
		return "group"
	case t.Chmod != nil:
		return "chmod"
	case t.Chown != nil:
		return "chown"
	case t.Download != nil:
		return "download"
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
	case t.MysqlDatabase != nil:
		return "mysql_database"
	case t.PgDatabase != nil:
		return "pg_database"
	case t.Use != "":
		return "use"
	default:
		return ""
	}
}
