// Package protocol defines the wire format between the CLI and the remote agent.
//
// Wire model (P2): the CLI writes a single JSON-encoded Plan to the agent's
// stdin and closes the stream. The agent emits a stream of newline-delimited
// Event objects on stdout, terminating with type=done. File bodies for
// copy/template tasks are carried inline (base64) in the Plan because configs
// are typically KB-sized; streaming back-channel is reserved for later.
//
// Anything on the agent's stderr is treated as unstructured crash/diagnostic
// noise by the CLI and surfaced as-is.
package protocol

const (
	EventTaskStart = "task_start"
	EventTaskEnd   = "task_end"
	EventLog       = "log"
	EventDone      = "done"

	StatusOK          = "ok"
	StatusChanged     = "changed"
	StatusSkipped     = "skipped"
	StatusError       = "error"
	StatusWouldChange = "would_change" // emitted in dry-run when a task would have applied a change
	StatusInSync      = "in_sync"      // status mode: file on disk matches state.json
	StatusDrift       = "drift"        // status mode: file exists but content/mode differs from state
	StatusMissing     = "missing"      // status mode: state.json knows the file but it's gone from disk

	TaskCommand       = "command"
	TaskCopy          = "copy"
	TaskTemplate      = "template"
	TaskMkdir         = "mkdir"
	TaskSymlink       = "symlink"
	TaskRemove        = "remove"
	TaskUser          = "user"
	TaskGroup         = "group"
	TaskChmod         = "chmod"
	TaskChown         = "chown"
	TaskDownload      = "download"
	TaskApt           = "apt"
	TaskService       = "service"
	TaskDockerCompose = "docker_compose"
	TaskDockerInstall = "docker_install"
	TaskDockerBuild   = "docker_build" // executed locally by the CLI, never sent to the agent
	TaskDockerLogin   = "docker_login" // executed both locally and on the agent depending on Location
)

type Plan struct {
	RulebookName string `json:"rulebook_name"`
	Phase        string `json:"phase"`
	DryRun       bool   `json:"dry_run,omitempty"`     // when true, handlers preview without applying
	Diff         bool   `json:"diff,omitempty"`        // dry-run + diff: agent attaches a unified diff to would_change events for copy/template
	StatusOnly     bool   `json:"status_only,omitempty"`     // when true, agent reports state.json drift instead of running tasks
	IncludeHistory bool   `json:"include_history,omitempty"` // only meaningful with StatusOnly: attach the per-file history chain to each status event
	KeepHistory    int    `json:"keep_history,omitempty"`    // when >0, copy/template archive the previous file body before overwrite and keep this many versions in state.json (for `axup rollback`)
	Rollback       bool   `json:"rollback,omitempty"`        // when true, agent restores tracked files from their history chain instead of running tasks
	RollbackStep   int    `json:"rollback_step,omitempty"`   // how far back to restore (1 = previous version); ignored unless Rollback is set
	RollbackTask   string `json:"rollback_task,omitempty"`   // optional: restrict rollback to a single tracked path (dst path = task_id for copy/template)
	ClearHistory   bool   `json:"clear_history,omitempty"`   // when true, agent wipes every FileState.History + removes the history/ dir (irreversible)
	Tasks          []Task `json:"tasks"`
}

// HistoryItem is the protocol-side view of a stored HistoryEntry — what
// the CLI needs to render `axup status --history`. We deliberately omit
// ArchivedPath: that's an agent-internal detail, not user-facing.
type HistoryItem struct {
	Sha256     string `json:"sha256"`
	Mode       string `json:"mode"`
	RecordedAt string `json:"recorded_at"`
	Phase      string `json:"phase,omitempty"`
}

// Task is a single unit of work. The Type field discriminates the union.
// File-bearing tasks (copy, template) carry their fully-resolved content in
// BodyB64; the agent verifies Sha256 and updates state when it writes.
type Task struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Type string `json:"type"`

	// command:
	Command string `json:"command,omitempty"`

	// copy / template / download (shared file-target fields):
	DstPath string `json:"dst_path,omitempty"`
	Mode    string `json:"mode,omitempty"`   // octal string, e.g. "0644"; empty = 0644
	Sha256  string `json:"sha256,omitempty"` // hex digest of decoded body (copy/template) OR expected sha of downloaded payload (download — optional)
	BodyB64 string `json:"body_b64,omitempty"`

	// mkdir:
	MkdirPath  string `json:"mkdir_path,omitempty"`
	MkdirOwner string `json:"mkdir_owner,omitempty"` // username; empty = no chown
	MkdirGroup string `json:"mkdir_group,omitempty"` // group name; empty = no chgrp

	// symlink:
	SymlinkSrc   string `json:"symlink_src,omitempty"`   // the path the link points TO
	SymlinkDst   string `json:"symlink_dst,omitempty"`   // the symlink itself
	SymlinkForce bool   `json:"symlink_force,omitempty"` // replace existing dst (even if it's a regular file)

	// remove:
	RemovePath      string `json:"remove_path,omitempty"`
	RemoveRecursive bool   `json:"remove_recursive,omitempty"` // dirs require this; without it, rmdir-like (errors on non-empty dirs)

	// user:
	UserName       string   `json:"user_name,omitempty"`
	UserShell      string   `json:"user_shell,omitempty"`       // default /usr/sbin/nologin (system users)
	UserHome       string   `json:"user_home,omitempty"`        // when set, passed as --home-dir
	UserCreateHome bool     `json:"user_create_home,omitempty"` // pass --create-home / --no-create-home
	UserGroups     []string `json:"user_groups,omitempty"`      // supplementary groups, reconciled via usermod -aG each run
	UserState      string   `json:"user_state,omitempty"`       // present (default) | absent

	// group:
	GroupName  string `json:"group_name,omitempty"`
	GroupState string `json:"group_state,omitempty"` // present (default) | absent

	// chmod / chown (target path reuses DstPath; chmod reuses Mode):
	ChownOwner     string `json:"chown_owner,omitempty"`
	ChownGroup     string `json:"chown_group,omitempty"`
	ChownRecursive bool   `json:"chown_recursive,omitempty"` // walks the tree with chown -R semantics

	// download:
	DownloadURL     string            `json:"download_url,omitempty"`
	DownloadHeaders map[string]string `json:"download_headers,omitempty"` // optional HTTP headers (e.g. Authorization)

	// apt:
	AptPackages    []string `json:"apt_packages,omitempty"`
	AptState       string   `json:"apt_state,omitempty"`        // present|absent
	AptUpdateCache bool     `json:"apt_update_cache,omitempty"` // run `apt-get update` first

	// service:
	ServiceName     string `json:"service_name,omitempty"`
	ServiceState    string `json:"service_state,omitempty"`    // started|stopped|restarted|reloaded
	ServiceEnabled  *bool  `json:"service_enabled,omitempty"`  // systemd only
	ServiceProvider string `json:"service_provider,omitempty"` // systemd|supervisor

	// docker_compose:
	ComposeDir   string `json:"compose_dir,omitempty"`
	ComposeState string `json:"compose_state,omitempty"` // up|down|restarted|pulled
	ComposePull  bool   `json:"compose_pull,omitempty"`  // pull before up
	ComposeWait  bool   `json:"compose_wait,omitempty"`  // pass --wait to `docker compose up` (block until healthchecks pass)

	// docker_build (CLI-local):
	BuildContext    string            `json:"build_context,omitempty"`
	BuildDockerfile string            `json:"build_dockerfile,omitempty"` // default "Dockerfile"
	BuildTags       []string          `json:"build_tags,omitempty"`       // one or more -t arguments
	BuildPush       bool              `json:"build_push,omitempty"`
	BuildPlatform   string            `json:"build_platform,omitempty"` // e.g. linux/amd64
	BuildArgs       map[string]string `json:"build_args,omitempty"`

	// docker_login (CLI-local and/or agent):
	LoginRegistry string `json:"login_registry,omitempty"`
	LoginUsername string `json:"login_username,omitempty"`
	LoginPassword string `json:"login_password,omitempty"`

	// Gate the task on whether any of these remote paths were changed this run.
	// Applies to command/service/docker_compose/apt; redundant on copy/template.
	WhenChanged []string `json:"when_changed,omitempty"`
}

type Event struct {
	Type     string        `json:"type"`
	TaskID   string        `json:"task_id,omitempty"`
	Status   string        `json:"status,omitempty"`
	Message  string        `json:"message,omitempty"`
	Stdout   string        `json:"stdout,omitempty"`
	Stderr   string        `json:"stderr,omitempty"`
	ExitCode int           `json:"exit_code,omitempty"`
	Path     string        `json:"path,omitempty"`    // for file-related events: which path was touched
	Diff     string        `json:"diff,omitempty"`    // unified diff, attached by agent when Plan.Diff && status=would_change
	History  []HistoryItem `json:"history,omitempty"` // attached by agent when Plan.IncludeHistory && status_only: newest first
}
