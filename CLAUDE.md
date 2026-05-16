# CLAUDE.md

Notes for Claude (and any other contributor) when modifying this repository.
For end-user docs see [README.md](README.md) and [doc/](doc/).

## What this is

`axup` is a single Go binary that reads YAML rulebooks and applies them to
Linux servers over SSH. The CLI uploads an embedded `axupd` agent into
`/tmp` on every run, pipes a JSON plan into its stdin, reads newline-delimited
events from stdout, and the agent maintains `~/.axup-state/<rulebook>/state.json`
on the remote so re-runs are idempotent (sha256 diffs on file contents + mode).

Two binaries, one repo:

```
cmd/axup/main.go       CLI shipped to developers (~12 MB, embeds the agent)
cmd/axupd/main.go      Remote agent cross-compiled for linux/amd64+arm64 (~2.3 MB)
```

`make` cross-compiles both agent variants, places them in `internal/agentbin/bin/`,
then builds the CLI with `//go:embed bin/axupd-linux-*` picking them up.
The order matters — building the CLI without the agents present fails the embed.

## Architecture map

```
cmd/
├── deploy/main.go             CLI entrypoint; wires cli.Execute
└── axupd/main.go            Agent entrypoint; calls agent.Run(stdin, stdout)

internal/
├── cli/                       cobra subcommands (one file per command)
│   ├── root.go                persistent flags (--key, --sudo, --age-key, --check, --no-color)
│   ├── bootstrap.go deploy.go status.go
│   ├── deps.go                deps tidy/verify
│   ├── secrets.go             secrets encrypt/decrypt/edit/status
│   ├── init.go version.go auth.go
│
├── runner/                    orchestrator: rulebook -> plan -> SSH -> events
│   ├── runner.go              Run(), buildPlans(), runOnHost(), printEvent
│   ├── color.go               ANSI helpers + TTY detection
│   └── summary.go             per-host counter + line formatter
│
├── rulebook/                  YAML parsing + expansion + secrets/deps integration
│   ├── types.go               Rulebook, Task, *Spec types
│   ├── parse.go               Load() + validateTasks() pipeline
│   ├── render.go              text/template + sprig; renderString, expandTaskStrings
│   ├── git.go                 git_sha / git_short_sha / git_branch / git_dirty auto-vars
│   ├── deps.go                external deps git resolver + cache layout
│   ├── lock.go                axup.lock load/save/reconcile
│   ├── expand.go              use: inlining + cycle detection
│   └── creds.go               docker_login creds_file loader (incl. age decrypt)
│
├── inventory/                 inventory.yaml parser + Resolve(--host, --group)
│   └── inventory.go
│
├── secrets/                   age encryption layer
│   └── secrets.go             Encrypt/Decrypt/LoadIdentities/LoadRecipients
│
├── transport/                 SSH client (golang.org/x/crypto/ssh)
│   ├── ssh.go                 Dial, UploadBinary, RunAgent
│   ├── auth.go                key/password discovery
│   ├── host.go                "user@addr[:port]" parser
│   └── knownhosts.go          known_hosts callback + algorithm scan
│
├── protocol/                  shared wire types
│   └── messages.go            Plan, Task, Event + status/event constants
│
├── local/                     CLI-side task executors (docker_build, docker_login)
│   └── exec.go
│
├── agent/                     remote agent runtime
│   ├── runner.go              Run(stdin, stdout) + executeTask dispatch
│   ├── tasks.go               command / file (copy+template) handlers + runCtx
│   ├── tasks_apt.go tasks_service.go tasks_compose.go
│   ├── tasks_docker_install.go tasks_docker_login.go
│   ├── state.go               state.json load/save (atomic write)
│   └── status.go              StatusOnly mode (emitStatus)
│
└── agentbin/                  go:embed of cross-built axupd binaries
    ├── embed.go
    └── bin/                   populated by `make agent` (gitignored)
```

## Rulebook phases

A rulebook top-level YAML maps to `rulebook.Rulebook`. The reserved keys
are `name`, `vars`, `deps`, `secrets`, and `tasks` (module-form only).
**Every other top-level key is a phase** and gets captured into
`Rulebook.Phases map[string][]Task` via `yaml:",inline"`. So:

```yaml
name: cubeweb
vars:    { ... }
bootstrap:    [...]      # → rb.Phases["bootstrap"]
deploy:       [...]      # → rb.Phases["deploy"]
deploy_crash: [...]      # → rb.Phases["deploy_crash"]
migrate:      [...]      # → rb.Phases["migrate"]
```

Phase names must match `^[a-z][a-z0-9_-]*$` and cannot be `status` or
`tasks` (both CLI-reserved). The CLI dispatches with:

- `axup bootstrap` — runs `bootstrap:` (backward-compat alias)
- `axup deploy` — runs `deploy:` (backward-compat alias)
- `axup run <phase>` — runs any phase (incl. bootstrap/deploy)

All three flow through the same `runner.Run`. State on the remote lives
at `~/.axup-state/<rulebook_name>/state.json` — every phase of the
same rulebook shares one state file: a `copy:` dst written by
`bootstrap` is "in sync" when a later `deploy_crash` runs and references
the same path. If you want isolated state, give each rulebook a
different `name:` (split into multiple rulebook files).

The `--vars <file>` flag merges an external YAML dict into rb.Vars.
Precedence (highest wins):

```
inventory host vars > --vars file > git auto-vars > rulebook vars defaults
```

Relative `--vars` paths resolve against CWD first, then against the
rulebook's directory (so `--vars vars.yaml` works regardless of where
the CLI was invoked from, as long as the file sits next to the rulebook).

## Services catalog + `axup logs`

The optional top-level `services:` block declares a name → log paths
map that `axup logs <name> [<name>...]` uses to tail files over SSH:

```yaml
services:
  kv:         { logs: /var/log/supervisor-kv.log }
  crash:      { logs: [/var/log/supervisor-crash.log, /var/log/cubeweb/crash-extra.log] }
  supervisor: { logs: /var/log/supervisor/supervisord.log }
```

Paths are templated through `rb.Vars` (so `{{ .log_dir }}/kv.log`
works) and `Logs` accepts either a string or a list via `StringOrList`.
The catalog is purely informational — it does NOT manage processes
(that's the `service:` task primitive, separate concept). Field lives
under `rb.Services map[string]Service` and is reserved (won't fall
into `Phases` even though it's a top-level key).

`axup logs` opens one SSH session per resolved host, runs
`tail -n N -q [-F] /path1 /path2 …`, and forwards stdout line-by-line
with `[host]` prefix. Defaults: `-n 20 -F`. Cancellation closes the
session — local Ctrl-C kills the remote `tail`.

## Wire protocol

CLI → agent: one `protocol.Plan` JSON object on stdin (the SSH session's
stdin). Agent reads to EOF, then writes events.

Agent → CLI: newline-delimited `protocol.Event` JSON on stdout. One
`task_start` + one `task_end` per task, optional `log` events, terminated
by a `done` event.

`Plan` flags that change agent behavior:
- `dry_run`: handlers do their idempotency check but skip the apply step and
  report `would_change` instead of `changed`. State is NOT written.
- `status_only`: agent skips tasks entirely, walks `state.json`, emits one
  synthetic `task_end` per tracked file with `in_sync` / `drift` / `missing`.

The Plan can carry inline base64-encoded file bodies (`copy` / `template`
`body_b64` field). Bodies are inlined because configs are KB-sized; we
deliberately don't have a streaming back-channel.

Sudo flow: when `--sudo` or `--sudo-password` is set, `RunAgent` prefixes
the remote command with `sudo -H -S -p ''`. The sudo password (if any) is
written to stdin BEFORE the Plan JSON; sudo consumes the first line, the
agent reads the rest. `-H` keeps `$HOME=/root` so state lives at
`/root/.axup-state/...` regardless of the SSH login user.

## How to add a new task type

Walkthrough using a hypothetical `firewall` task as the running example.
Touch all of:

1. **`internal/protocol/messages.go`** — add a `TaskFirewall = "firewall"`
   constant and Plan-level fields the agent needs (`FirewallRules []string`,
   `FirewallDefault string`, etc).

2. **`internal/rulebook/types.go`** — add `Firewall *FirewallSpec` to `Task`
   and define the spec struct. Add a `case t.Firewall != nil:` branch in
   `Kind()`.

3. **`internal/rulebook/parse.go`** — extend `validateTasks` with field-level
   checks (required fields, allowed enum values, etc). Bump the
   "expected one of …" error string in the `set == 0` branch.

4. **`internal/rulebook/render.go`** — if the spec has any user-facing
   strings, add them to `expandTaskStrings` so `{{ .var }}` resolves. **Don't
   forget src paths** if there are file references (we have a history of
   missing those — see [commit b5ee754](https://github.com/AxGrid/axup/commit/b5ee754)).

5. **`internal/runner/runner.go`** — add a `case "firewall":` branch in
   `buildPlans` to translate the rulebook task into a `protocol.Task`.
   Decide whether the task is local (append to `localTasks`) or remote
   (`remoteTasks`).

6. **Either `internal/local/exec.go` OR `internal/agent/tasks_*.go`** —
   write the handler. Remote handlers take `(ctx *runCtx, t protocol.Task)`
   and return `protocol.Event`. Local handlers take `(t protocol.Task, dryRun bool)`.

7. **`internal/agent/runner.go`** OR **`internal/local/exec.go`** — wire
   the new case into the dispatch switch.

8. **Dry-run support** — every handler MUST check `ctx.dryRun` (remote) or
   the `dryRun` arg (local). Return `StatusWouldChange` with a "would …"
   message before any state-mutating call. The agent's `state.save()` is
   gated on `!ctx.dryRun` at the runner level, so handler-side state
   updates (`ctx.state.Files[...] = …`) only need to be conditional if
   they happen before the dry-run early return.

9. **Tests** — there's no test suite yet (MVP). Add a minimal example in
   `examples/<name>/rulebook.yaml` that exercises the new task, smoke-test
   against `root@cert2.axgrid.com` (see [memory: test_server](/Users/zed/.claude/projects/-Users-zed-GoLang-deploy/memory/test_server.md)).

10. **Docs** — append the new type to [doc/rulebook-reference.md](doc/rulebook-reference.md)
    with a YAML example and status semantics. Update the README's "9 task
    primitives" line to the new count.

## Conventions

- **Statuses**: `changed` (we did something), `skipped` (no-op, already in
  desired state), `error`, `would_change` (dry-run), and the three status-mode
  ones (`in_sync` / `drift` / `missing`). Don't invent new ones without a
  matching color in `runner/runner.go:paintByStatus`.

- **Idempotency first**: every handler must check current state before doing
  anything. `dpkg-query` for apt, `systemctl is-active` / `is-enabled` for
  systemd, sha256 + mode for files, `docker --version` for docker_install,
  `supervisorctl status` for supervisor. The skip path is what makes the
  tool nice to use.

- **No external binary deps on the remote** other than the standard ones
  the task explicitly invokes (`apt-get`, `systemctl`, `docker`, etc).
  Don't shell out to `awk` / `sed` / `jq` etc — write the parsing in Go.

- **Paths in tasks**: relative `src:` / `dst:` / `context:` paths in
  top-level tasks are relative to the rulebook directory (`rb.Dir`).
  Spliced module tasks have their relative paths rewritten to absolute by
  `expand.go:rewriteRelativePaths`. Don't make a handler care — the loader
  always gives it an absolute path.

- **Template-rendered fields**: anything user-facing that's a string must
  go through `expandTaskStrings` in `render.go`. Test by referencing a
  per-host var like `{{ .env }}` — if it's not rendering you forgot to add
  the field there.

- **EffectiveVars for module-derived tasks**: when a task came from `use:`,
  `Task.EffectiveVars` is non-nil and holds the merged map (module defaults
  < caller < pre-rendered use.vars). The runner uses it for template-body
  rendering. If you add a handler that renders strings late, plumb
  `EffectiveVars` through too.

- **Output**: anything user-visible goes through `runner.printEvent` or
  `runner.printLine` so the per-host mutex serializes lines under parallel
  multi-host fan-out. Don't `fmt.Println` directly from `Run`/`runOnHost`.

- **Errors**: wrap with context using `fmt.Errorf("[%s] %w", host.Name, err)`
  in `runOnHost`. The host tag prefix gives users somewhere to look first.

## Build, test, verify

```sh
make                # cross-build agent + CLI; produces ./bin/axup
make agent          # just the agent binaries (internal/agentbin/bin/axupd-linux-*)
go build ./...      # quick "does it compile" check (CLI build fails without the agents)
```

There's no `go test ./...` yet — the project relies on end-to-end smoke
tests against a real server. The user's primary test target is
`root@cert2.axgrid.com` (Ubuntu, x86_64, root SSH via key auth).

Typical smoke test for a new task type:

```sh
make
ssh root@cert2.axgrid.com 'rm -rf /opt/<your-app> /root/.axup-state/<name>'
./bin/axup bootstrap --host root@cert2.axgrid.com --rulebook examples/<name>/rulebook.yaml
./bin/axup bootstrap --host root@cert2.axgrid.com --rulebook examples/<name>/rulebook.yaml  # 2nd run — expect skipped
./bin/axup status    --host root@cert2.axgrid.com --rulebook examples/<name>/rulebook.yaml
./bin/axup bootstrap --check --host root@cert2.axgrid.com --rulebook examples/<name>/rulebook.yaml
```

Cleanup pattern at the end of a session:

```sh
ssh root@cert2.axgrid.com 'rm -rf /opt/<your-app> /root/.axup-state/<name>'
```

## Common gotchas

- **`go:embed` requires the bin/ to exist at build time.** Running
  `go build ./cmd/axup` without first building the agent fails. `make`
  handles the ordering. There are 0-byte placeholder files committed so
  embed at least finds something — the CLI errors at runtime with "agent
  binary for amd64 not built" if you forgot `make agent`.

- **`secrets/` is gitignored.** Encrypted files are safe to commit, but
  the global pattern catches them. For projects encrypting via this tool,
  either drop `secrets/` from the gitignore or move encrypted files to
  `enc/`.

- **`docker_compose up` always reports `changed`.** Parsing `docker compose ps`
  reliably across versions is brittle. Don't add a "check if up" heuristic
  without a strong reason. If a task downstream needs the container to be
  *ready* (not just running), set `wait: true` on the `docker_compose` task —
  the agent appends `--wait` to `docker compose up`, blocking until every
  service's healthcheck reports `healthy`. Works only for `state: up`.

- **`when_changed` on copy/template is rejected.** Those primitives already
  have sha-based skip logic. The parser errors out with a clear message.
  Don't try to relax this — it'd be confusing in two directions at once.

- **Module rulebooks cannot have their own `deps:`.** Transitive deps are
  out of scope for the MVP. Enforced at parse time in `expand.go:loadModule`.

- **`HostKeyAlgorithms` in known_hosts.** Go's ssh package picks the
  server's first offered algo by default; if `~/.ssh/known_hosts` only has
  the server's ed25519 line and the server happens to offer ecdsa first,
  you get a spurious "key mismatch". `transport/knownhosts.go:scanHostAlgorithms`
  extracts the algos from the file and passes them to `ClientConfig` so the
  handshake negotiates the right type.

- **State path uses HOME, sudo needs -H.** When `--sudo` is used, the
  agent runs as root but `$HOME` is preserved from the invoking user
  unless `sudo -H` is set. We always use `-H` so state lives at
  `/root/.axup-state/...`.

## Where to look first

- A task is misbehaving on the remote? Read `internal/agent/tasks_<type>.go`
  for the handler, then `internal/agent/state.go` for state interaction.
- A flag isn't recognized? `internal/cli/root.go` for persistent flags,
  `internal/cli/<subcommand>.go` for command-local flags.
- A `{{ .var }}` isn't expanding? `internal/rulebook/render.go:expandTaskStrings`
  — make sure the field is listed.
- A use:-imported module's template is rendering with the wrong vars?
  `internal/runner/runner.go:buildPlans` "template" case — make sure it
  prefers `t.EffectiveVars` over `rb.Vars`.
- A status mark is missing color? `internal/runner/runner.go:paintByStatus`.

## Memory and skills

User-private memory for this project lives at
`/Users/zed/.claude/projects/-Users-zed-GoLang-deploy/memory/`. It's auto-loaded
into Claude's context — don't duplicate its contents here. CLAUDE.md is the
trackable, repository-shipped equivalent.

A `/axgrid-deploy` skill at `~/.claude/skills/axgrid-deploy/SKILL.md`
scaffolds a fresh `rulebook.yaml` for new projects that want to adopt this
tool. Update both this file AND the skill when adding a new task type or
changing a rulebook surface.
