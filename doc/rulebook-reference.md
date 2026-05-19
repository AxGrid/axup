# Rulebook reference

A rulebook is the YAML file (`rulebook.yaml` by convention) that drives an
`axup` invocation. It declares the project's name, top-level variables,
optional external dependencies, optional secrets layout, and two task lists:
`bootstrap:` (called by `axup bootstrap`) and `deploy:` (called by
`axup deploy`).

## IDE support — JSON Schema

A JSON Schema ships in [`schemas/rulebook.schema.json`](../schemas/rulebook.schema.json)
and is published at
`https://raw.githubusercontent.com/AxGrid/axup/main/schemas/rulebook.schema.json`.
The [yaml-language-server](https://github.com/redhat-developer/yaml-language-server)
(VS Code's official YAML extension, JetBrains' YAML plugin, neovim's
`vim-lsp`, …) picks it up from this header at the top of the file:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/AxGrid/axup/main/schemas/rulebook.schema.json
name: my-app
…
```

`axup init` writes that header automatically. Once it's present you get
autocomplete on task types, validation of required fields (e.g. forgetting
`dst:` on `copy:`), enum hints (`state: started|stopped|restarted|reloaded`),
and inline docs on hover.

## Top-level structure

```yaml
name: my-app                          # required; used as the state-dir name on the remote

history: 3                            # optional; keep the last N versions of every copy/template
                                      # file on the remote so `axup rollback` can restore them.
                                      # 0 (default) disables history capture. See "Rollback
                                      # history" below.

vars:                                 # optional; available as {{ .var_name }} in templates
  app_name: my-app
  port: 8080

deps:                                 # optional; see external-rulebooks.md
  - { name: common, git: github.com/me/deploy-rules, version: v1.4.2 }

secrets:                              # optional; see secrets.md
  recipients_file: recipients.txt
  files:
    - secrets/registry.com.yaml

bootstrap:                            # tasks run by `axup bootstrap`
  - { command: "apt-get update -qq" }
  - …

deploy:                               # tasks run by `axup deploy`
  - { docker_compose: { dir: /opt/my-app, state: up } }
  - …

# Any other top-level key is a custom phase. Run it with
# `axup run <phase>`.
deploy_crash:
  - { copy: { src: bin/crash, dst: /opt/crash } }
  - { service: { name: crash, state: restarted, provider: supervisor } }

migrate:
  - { command: "/opt/my-app/bin/migrate up" }
```

The `name` field doubles as the remote state directory: `~/.axup-state/<name>/state.json`.
It must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`.

## Phases

Reserved top-level keys (these have their own meaning in the schema and
are NOT treated as phases): `name`, `vars`, `deps`, `secrets`, `services`,
`history`, `tasks` (module-form only).

Everything else is a phase. Phase names must match `^[a-z][a-z0-9_-]*$`
and cannot collide with one of the CLI dispatch words: `status`, `tasks`,
`services`, `logs`, `history`, `rollback`.

Run any phase via:

```
axup bootstrap                # alias for: axup run bootstrap
axup deploy                   # alias for: axup run deploy
axup run deploy_crash         # any custom phase
axup run migrate --check      # dry-run is supported on every phase
axup run axup --group prod  # all the existing flags apply
```

All phases of the same rulebook share one state file
(`~/.axup-state/<name>/state.json`) on the remote — so a `copy:` dst
written during `bootstrap` is "already in sync" when a later
`deploy_crash` references the same path. If you want isolated state,
split into multiple rulebook files with different `name:` values.

## External vars (`--vars`)

`--vars <path>` loads a YAML dict and merges it into `vars:` after the
rulebook's own defaults and git auto-vars, but before per-host
inventory vars. Useful when you want to keep secrets / per-env knobs
out of the rulebook itself:

```yaml
# vars.prod.yaml
admin_password: "{{ env.ADMIN_PASSWORD }}"
mysql_password: "{{ env.MYSQL_PASSWORD }}"
```

```
axup deploy --vars vars.prod.yaml --group prod
```

Precedence (highest wins):

```
inventory host vars  >  --vars file  >  git auto-vars  >  rulebook vars
```

Relative `--vars` paths resolve against CWD first, then against the
rulebook's directory.

## Services catalog (`services:`)

The optional top-level `services:` block declares a name → log files
map consumed by [`axup logs`](cli-reference.md). It does NOT manage
processes — that's still the `service:` task primitive. Think of it as
"the catalog of things you might want to tail".

```yaml
services:
  kv:           { logs: /var/log/supervisor-kv.log }
  billing:      { logs: /var/log/supervisor-billing.log }
  rng:          { logs: [/var/log/supervisor-rng.log, /var/log/rng-extra.log] }
  supervisor:   { logs: /var/log/supervisor/supervisord.log }
```

- `logs:` accepts a string or a list (same `StringOrList` convention
  as `apt: name:`).
- Paths are templated against `vars:` — so `{{ .log_dir }}/kv.log` works
  and resolves per host.
- Service names match `^[a-z][a-z0-9_-]*$`. `services` is reserved and
  cannot be used as a phase name.

`axup logs <name>... --host X` (or `--group X`) opens an SSH session
per resolved host, runs `tail -n 20 -q -F <paths…>` against the
resolved files, and streams output prefixed with `[host]`. See
[cli-reference.md](cli-reference.md) for flags (`-n` / `--tail`,
`--no-follow`, `--list`).

## Rollback history (`history:`)

Set `history: N` at the top level to make the agent archive the previous
body of every `copy:` / `template:` file before overwriting it. Up to N
previous versions are kept under `~/.axup-state/<name>/history/` on each
remote host, and recorded in `state.json` per file (newest first). N
must be in the range 0..50; default 0 means history capture is OFF.

```yaml
name: my-app
history: 3        # keep last 3 versions per tracked file
```

What the agent does on every `copy:` / `template:` overwrite:

1. Reads the current on-disk file, writes a copy to
   `~/.axup-state/<name>/history/<random>.bak` (mode 0600).
2. Prepends a `HistoryEntry` to `state.json`'s `files["<dst>"].history`
   array (sha + mode + recorded_at + phase + task_id).
3. Evicts the oldest entry beyond N and `rm`'s its archive file.

Fresh writes (where the file didn't exist yet) don't produce a history
entry — there's nothing to archive. No-op runs (when the on-disk file
already matches the desired sha + mode) also don't archive, but they
preserve any existing chain.

What you can do with the chain:

| Command | Purpose |
|---|---|
| `axup status --history` | Print every file's chain (sha, mode, recorded_at, phase). Read-only. |
| `axup rollback [--step N] [--task PATH]` | Restore each file from `history[N-1]`. Reset-semantics: rolled-over entries are dropped. |
| `axup history clear` | Wipe every chain + remove the history dir on each host. Irreversible. |

See [cli-reference.md](cli-reference.md) for the full flag set on each.

### Operational notes

- **Reset semantics on rollback.** Two `axup rollback --step 1` calls go
  back two versions, not back-then-forward. Documented in the command's
  `--help`; aligns with how ops "go back to vN" mental-models work.
- **No history capture for fresh writes.** The very first deploy of a
  file produces no history (nothing existed to archive). Plan accordingly
  when scripting "deploy then rollback" smoke tests.
- **Use `axup history clear` after irreversible deploys.** When a phase
  ran something that breaks rollback at runtime (DB migration, on-disk
  format change), the previous binary in history is *worse* than no
  history — `axup rollback` would happily restore it and then break
  startup. The standard recipe is:
  ```
  axup deploy   --host h --rulebook r
  axup history clear --host h --rulebook r --yes
  ```
- **Disk cost.** Three copies of a 100 MB binary across 5 hosts = 1.5 GB
  total. Plan capacity, or drop `history:` for projects where individual
  files are huge.
- **No retroactive enable.** Setting `history: 3` only affects future
  overwrites — past writes can't be archived after the fact.
- **Per-host chains.** Each host has its own `state.json` + history dir,
  so a multi-host group can have divergent chains (one host's deploy
  failed and rolled back, others didn't). `axup status --history --group X`
  prints them side by side prefixed by `[hostname]`.

## Variables and templating

Every string field in a task that contains `{{ … }}` is rendered as a Go
`text/template` against the rulebook's variables, plus the [sprig](https://masterminds.github.io/sprig/)
function library. This applies to `command`, both `src` and `dst` paths,
service names, compose dirs, build tags, Dockerfile paths, `creds_file`,
`password_file`, and so on — not just template-file bodies. So a single
rulebook task can serve every host in a group by reading a different file
per host:

```yaml
# rulebook.yaml
- copy:
    src: "files/env.{{ .env }}.conf"          # prod hosts read files/env.prod.conf
    dst: /opt/app/.env                         # stage hosts read files/env.stage.conf
```

```yaml
# inventory.yaml
hosts:
  prod-1:  { address: 10.1.1.10, vars: { env: prod } }
  stage-1: { address: 10.2.1.10, vars: { env: stage } }
```

Built-in variables automatically merged into `vars:` if not already set:

| Variable | Meaning |
|---|---|
| `git_sha` | full SHA of the current HEAD, or `""` if the project isn't a git repo |
| `git_short_sha` | first 7 characters of `git_sha` |
| `git_branch` | current branch name, or `""` on detached HEAD |
| `git_dirty` | `"true"` if `git status --porcelain` is non-empty |

Per-host variables from `inventory.yaml` are merged last and override the
rulebook's defaults. See [inventory-multi-host.md](inventory-multi-host.md).

## Task types

Quick reference — every task type at a glance, then a detailed section for
each below:

| Task | Runs on | Purpose | Required fields |
|---|---|---|---|
| [`command`](#command) | remote | Shell command via `/bin/sh -c` | `command` |
| [`copy`](#copy) | remote | Literal file from rulebook dir → remote path | `src`, `dst` |
| [`template`](#template) | remote | Go template (+ sprig) → remote path | `src`, `dst` |
| [`mkdir`](#mkdir) | remote | Create directory; reconcile mode/owner/group | `path` |
| [`symlink`](#symlink) | remote | Create / update a symbolic link | `src`, `dst` |
| [`remove`](#remove) | remote | Delete a path (file / dir / symlink), idempotent | `path` |
| [`user`](#user) | remote | Create / delete a system user; reconcile supplementary groups | `name` |
| [`group`](#group) | remote | Create / delete a system group | `name` |
| [`chmod`](#chmod) | remote | Set POSIX mode on an existing path | `path`, `mode` |
| [`chown`](#chown) | remote | Set owner / group on an existing path (optionally recursive) | `path`, `owner` or `group` |
| [`download`](#download) | remote | HTTP GET → file on the remote, optional sha256 verification | `url`, `dst` |
| [`apt`](#apt) | remote | Install / remove Debian packages | `name` (or `update_cache: true`) |
| [`service`](#service) | remote | Manage a systemd or supervisor unit | `name` |
| [`docker_install`](#docker_install) | remote | Install Docker Engine via `get.docker.com` | — |
| [`docker_compose`](#docker_compose) | remote | `docker compose up` / `down` / `restarted` / `pulled` | `dir` |
| [`docker_build`](#docker_build-cli-local) | CLI host | Build image with `docker buildx`, optional `push:` | `context`, one of `tag` / `tags` |
| [`docker_login`](#docker_login-cli-local-andor-remote) | both (default) / local / remote | `docker login` against a private registry | `registry`, `creds_file` (or inline) |
| [`mysql_database`](#mysql_database) | remote | `CREATE DATABASE` (+ optional user/grant) via the `mysql` CLI | `name`, `host`, `admin_user`, `admin_password` |
| [`pg_database`](#pg_database) | remote | `CREATE DATABASE` (+ optional role/grant) via the `psql` CLI | `name`, `host`, `admin_user`, `admin_password` |

Common keys every task may carry:

| Key | Type | Applies to | Effect |
|---|---|---|---|
| `name` | string | every type | Human-readable label shown in CLI output (`▶ <name> (id)`). Defaults to `task #N`. |
| `when_changed` | list of remote paths | `command`, `apt`, `service`, `docker_compose`, `docker_install`, `docker_login`, `user`, `group` | Gate the task: it fires only if any of the listed paths were written by an earlier task this run. **Rejected on `copy` / `template` / `mkdir` / `symlink` / `remove` / `chmod` / `chown` / `download`** — those each stat their own path and decide skip-vs-apply on their own. |
| `use` | `<dep>/<module_path>` | (special) | Splice a module from a `deps:` entry at this position. Mutually exclusive with the primitives above. See [external-rulebooks.md](external-rulebooks.md). |

Status semantics (what each task can return in events):

| Status | Meaning |
|---|---|
| `changed` | The task did real work — file written, package installed, service restarted, image built, etc. |
| `skipped` | The desired state already held — nothing to do. |
| `error` | The task failed; the per-host run is marked failed but other tasks still run (you'll see the summary line). |
| `would_change` | Dry-run (`--check`): the task WOULD have done something. |

### `command`

Run a shell command on the remote via `/bin/sh -c`.

```yaml
- name: clear apt list cache
  command: "rm -rf /var/lib/apt/lists/*"

- name: reload nginx if its config changed
  command: "systemctl reload nginx"
  when_changed:
    - /etc/nginx/nginx.conf
```

Status: `changed` on exit code 0, `error` otherwise. Without `when_changed`
the command runs on every invocation.

#### Conditional execution + ignored errors

The full form gates the command on a shell predicate and lets you ignore
non-zero exits, in the spirit of Ansible's `creates:` / `removes:` and
`failed_when:`:

```yaml
- name: ensure swap
  command:
    run: |
      fallocate -l 4G /swapfile && chmod 600 /swapfile
      mkswap /swapfile && swapon /swapfile
      echo '/swapfile none swap sw 0 0' >> /etc/fstab
    unless: swapon --show | grep -q .   # skip if swap already exists

- name: snapshot before deploy
  command:
    run: tar -czf /var/backups/data-{{ .git_short_sha }}.tar.gz /opt/myapp/data
    when: test -d /opt/myapp/data       # only when there's data to snapshot

- name: stop kiosk if it's running
  command:
    run: supervisorctl stop kiosk
    ignore_errors: true                 # "|| true" — don't fail if absent
```

| Field | Meaning |
|---|---|
| `run` | The shell command body. Multi-line strings are fine (pass to `/bin/sh -c`). |
| `when` | Run the body only if this shell predicate exits 0. Reports `skipped` otherwise. |
| `unless` | Inverse of `when`: run only if the predicate exits non-zero. Reports `skipped` otherwise. |
| `ignore_errors` | Non-zero exit becomes `changed` with an "ignored exit=N" message instead of `error`. |

`when` and `unless` are mutually exclusive. Predicates run on the remote via
`/bin/sh -c` and should be cheap, read-only sniffs (`test -f …`, `swapon --show
| grep -q .`, `systemctl is-active nginx`).

### `copy`

Send a literal file from the rulebook's directory to a remote path. Idempotent
by sha256 + mode.

```yaml
- name: install ssl certificate
  copy:
    src: files/ssl.crt           # relative to rulebook.yaml's dir
    dst: /etc/ssl/private/ssl.crt
    mode: "0600"                 # optional; default 0644
```

Status: `changed` when the file content or mode differed and was written,
`skipped` when the on-disk file already matches.

### `template`

Render a Go template file with the rulebook's vars, then deliver it like
`copy`.

```yaml
- name: write supervisor conf
  template:
    src: templates/myapi.supervisor.conf.tmpl
    dst: /etc/supervisor/conf.d/myapi.conf
    mode: "0644"
```

Inside the template you have full access to vars + sprig:

```ini
[program:{{ .app_name }}]
command=/usr/local/bin/myapi --port={{ .port | default 8080 }}
autostart=true
autorestart=true
environment=ENV={{ env "DEPLOY_ENV" | default "prod" }}
```

Status: same semantics as `copy`. The render happens on the CLI host using the
host's vars; the agent writes the resulting bytes atomically.

### `mkdir`

Create a directory on the remote. Always behaves like `mkdir -p` — parent
directories are created automatically. Mode, owner, and group are
reconciled on every run (drift in any of them triggers chmod / chown).

```yaml
# Shorthand: just a path. Mode defaults to 0755, no chown.
- mkdir: /opt/myapp

# Full form
- mkdir:
    path: /var/log/myapp
    mode: "0750"          # default 0755; must be a quoted octal string
    owner: app            # optional; user must already exist on the remote
    group: app            # optional; group must already exist
```

Idempotency:

- **Absent** → `mkdir -p` + chmod + optional chown → `changed`.
- **Present and is a directory, requested attributes match** → `skipped`.
- **Present and is a directory, an explicitly-set attribute drifts** →
  chmod / chown to reconcile → `changed`.
- **Present and is a regular file** → `error`. We never auto-`rm` to "fix"
  the conflict — use a `remove:` task before the `mkdir:` if that's
  intentional.

When `mode:` is omitted, `mkdir:` uses 0755 only on CREATE and does NOT
reconcile mode on subsequent runs. This lets a later `chmod:` task own
the mode without `mkdir:` fighting it on every re-run. Same shape for
`owner:` / `group:` — if you don't set them on `mkdir:`, it won't touch
ownership on an existing dir.

`owner` / `group` are resolved on the remote via `/etc/passwd` and
`/etc/group`. Failure to resolve a non-empty name returns a clear error
("resolve owner \"app\": ... — run earlier task to useradd it, or drop the
field") rather than silently chown'ing to uid `-1`.

### `symlink`

Create or reconcile a symbolic link. Typical use case is the blue-green
deploy pattern: each release lives in `/opt/myapp-v1.2.3/`, and a
`current` symlink points to the active one.

```yaml
- symlink:
    src: "/opt/myapp-{{ .release }}"   # what the link points TO
    dst: /opt/myapp/current             # the link itself
    force: true                          # default true — see below
```

Idempotency:

- **`dst` absent** → create symlink → `changed`.
- **`dst` is a symlink with the right target** → `skipped`.
- **`dst` is a symlink with a different target** → unlink + relink →
  `changed`.
- **`dst` is a regular file or directory** → `force: true` (default)
  removes it and creates the symlink; `force: false` returns `error`
  ("exists and is not a symlink").

`force: true` matches `ln -sfn` semantics — the common case where you
*want* to replace whatever's there. `force: false` is the safety belt
for cases where overwriting a real file at `dst` should be loud.
RemoveAll is used to clear `dst` under `force: true`, so a non-empty
directory at `dst` will be removed — passing `force: true` is the
explicit opt-in for that.

Parent directory of `dst` is auto-created (`MkdirAll`) so a fresh
`/opt/myapp/current` works without a prior `mkdir: /opt/myapp`.

### `remove`

Delete a path. Idempotent — absent paths report `skipped` instead of
erroring.

```yaml
# Shorthand
- remove: /opt/myapp/stale.flag

# Full form
- remove:
    path: /opt/myapp/old-releases
    recursive: true                     # default false; required for non-empty dirs
```

Idempotency:

- **Absent** → `skipped`.
- **Present (file or symlink)** → unlink → `changed`. Symlinks are
  unlinked; the symlink's target is left alone.
- **Present and is a directory, `recursive: false`** → `error` if
  non-empty (the underlying `os.Remove` returns `ENOTEMPTY`). Empty dirs
  remove cleanly.
- **Present and is a directory, `recursive: true`** → `rm -rf` semantics
  → `changed`.

`recursive: false` is the default specifically to avoid surprising `rm
-rf` accidents from a typo'd path — opt into it explicitly.

### `user`

Create or delete a system user on the remote. MVP scope is "exists or
not" — we do not reconcile shell, home directory, or uid on an
existing user (those changes have nasty failure modes for running
services that already own files under the old uid). Supplementary
groups ARE reconciled: adding a new group to the rulebook attaches it
on the next run via `usermod -aG`.

```yaml
# Shorthand — create system user "app" with /usr/sbin/nologin
- user: app

# Full form
- user:
    name: app
    shell: /bin/false               # default /usr/sbin/nologin
    home: /var/lib/app              # optional; passed as --home-dir
    create_home: true               # only meaningful with home: ; pairs with --create-home
    groups: [docker, ssl-cert]      # supplementary groups, reconciled each run
    state: present                  # present (default) | absent
```

Idempotency:

- **`state: present`**
  - Absent → `useradd -r [--shell] [--home-dir] [--create-home] [--groups]` → `changed`.
  - Present + all requested supplementary groups already attached → `skipped`.
  - Present + missing supplementary groups → `usermod -aG <missing>` → `changed`.
- **`state: absent`**
  - Present → `userdel -r` → `changed`.
  - Absent → `skipped`.

Always passes `-r` (creates a system user). For interactive users,
drop down to `command:`. Shell / home / create_home / groups are
ignored when `state: absent` (validated at parse time).

Groups must already exist on the target. Use the [`group`](#group)
task before any `user:` that references them via `groups:`.

### `group`

Create or delete a system group on the remote. Mirrors [`user`](#user)'s
MVP scope — exists-or-not, no gid pinning, no rename. Always uses
`-r` (system group) on create.

```yaml
# Shorthand
- group: docker

# Full form
- group:
    name: app
    state: present                  # present (default) | absent
```

Idempotency:

- **`state: present`** — present → `skipped`; absent → `groupadd -r` →
  `changed`.
- **`state: absent`** — present → `groupdel` → `changed`; absent →
  `skipped`.

Typical pattern: declare every group your services need at the top of
`bootstrap:`, then `user: { name: ..., groups: [...] }` further down
the file picks them up.

### `chmod`

Set POSIX permissions on an existing path. Idempotent — stat first,
chmod only if the current mode differs.

```yaml
- chmod:
    path: /etc/nginx/sites-enabled/foo
    mode: "0644"
```

Both `path:` and `mode:` are required. Errors if the path doesn't
exist (intentional — don't silently no-op on a typo). Symlinks are
followed (Stat semantics) — chmod on a symlink targets the link's
contents on Linux anyway, which matches the typical mental model.

No `recursive:` flag — applying a single mode to both directories and
files in a tree is rarely what you want (dirs usually need the
exec/search bit, files don't). For tree-wide perm management drop
to a `command:` task:

```yaml
- command: "find /opt/myapp -type d -exec chmod 0755 {} +"
- command: "find /opt/myapp -type f -exec chmod 0644 {} +"
```

### `chown`

Set owner / group on an existing path. At least one of `owner:` or
`group:` must be set.

```yaml
# Just owner
- chown: { path: /opt/myapp/data, owner: app }

# Owner + group
- chown:
    path: /opt/myapp/bin
    owner: app
    group: app

# Recursive: walk the tree and chown -RP every entry
- chown:
    path: /opt/myapp
    owner: app
    group: app
    recursive: true                    # default false
```

Idempotency: stat the TOP `path:`. If its current uid/gid already
matches the requested values, the task is `skipped` — INCLUDING the
recursive case. The assumption is "if the top is right, the tree is
right too", which holds when every chown on this tree has gone through
this task. Edge case: a manual chown by another admin on some
children won't trigger a re-run; force it by chowning the top to a
different user first, then back.

Recursive walk uses `Lchown` (chown -RP semantics) — owner of the
symlink itself, never its target. Without `recursive:` we also Lchown
the single path.

Owner / group are resolved on the remote via `/etc/passwd` and
`/etc/group`. Failure to resolve a non-empty name returns a clear
error rather than silently chowning to uid `-1` — pair `chown:` with
an upstream [`user`](#user) / [`group`](#group) task if the principal
doesn't exist yet.

### `download`

HTTP GET a URL into a file on the remote. Idempotency is driven by the
optional `sha256:` field — recommend pinning it for any non-trivial
use so a moving target (release "latest") never gets silently re-
fetched.

```yaml
# Public URL, sha pinned (recommended)
- download:
    url: https://github.com/foo/bar/releases/download/v1.2.3/bar-linux-amd64
    dst: /usr/local/bin/bar
    mode: "0755"                    # default 0644
    sha256: "abc123..."             # optional but recommended (64 hex chars)

# Private endpoint via Authorization header
- download:
    url: "https://api.example.com/internal/release.tgz"
    dst: /tmp/release.tgz
    headers:
      Authorization: "token {{ .gh_token }}"
      X-Trace-Id: "axup-{{ .git_short_sha }}"
```

Idempotency:

- **Absent** → fetch → write atomically (`<dst>.download.tmp` + rename)
  → optional `sha256` verification → chmod → `changed`.
- **Present + `sha256` set + matches on-disk file** → `skipped` (no
  network round-trip).
- **Present + `sha256` set + on-disk mismatch** → re-fetch and
  overwrite → `changed`.
- **Present + no `sha256`** → `skipped` (trust the existing file; doc
  recommends pinning for non-trivial use).
- **Present + content matches but mode drifted** → `chmod` only →
  `changed`.

Headers are templated through `vars:` like everywhere else, so secrets
can come via inventory host vars or `--vars file.yaml` rather than
being baked into the rulebook.

The agent uses `crypto/tls` for HTTPS (no `curl`/`wget` dependency on
the remote). Default timeout 10 minutes. Mode default 0644. Parent dir
of `dst` is auto-created.

### `apt`

Install or remove Debian/Ubuntu packages with `apt-get`. Idempotent via
`dpkg-query` pre-checks.

```yaml
- name: install supervisor
  apt:
    name: supervisor              # single package
    state: present                # default; or "absent"
    update_cache: true            # run apt-get update first

- name: install build chain
  apt:
    name: [build-essential, libssl-dev, pkg-config]

- name: refresh apt cache only
  apt:
    update_cache: true            # standalone form — no name required
```

`name:` accepts either a string or a list. `apt-get` is invoked with
`DEBIAN_FRONTEND=noninteractive` and `--no-install-recommends`. At least
one of `name:` or `update_cache: true` is required.

Status: `skipped` when every package is already in the desired state and
`update_cache` is false; otherwise `changed`.

### `service`

Manage a unit on either systemd (default) or supervisor. Idempotent via
`systemctl is-active` / `supervisorctl status`.

```yaml
# systemd
- service:
    name: docker
    state: started        # started | stopped | restarted | reloaded
    enabled: true         # optional; only valid for systemd

# supervisor
- service:
    name: my-worker
    state: started
    provider: supervisor

# reload supervisor's config after a template change
- service:
    name: my-worker
    state: reloaded       # supervisor: maps to `supervisorctl update`
    provider: supervisor
  when_changed:
    - /etc/supervisor/conf.d/my-worker.conf
```

**Supervisor auto-reload.** When `provider: supervisor` and the named program
is not present in `supervisorctl status` (typical right after a `template:` /
`copy:` drops a new `*.ini` into `/etc/supervisor/conf.d/`), the agent
automatically runs `supervisorctl reread && supervisorctl update` before
issuing `start` / `restart`, then re-checks status. You don't need a separate
`state: reloaded` task in front of every new program — declare the desired
state and the reload is implicit. For `state: stopped`, a not-yet-declared
program reports `skipped` ("nothing to stop") instead of erroring.

### `docker_install`

Install Docker Engine via the official `https://get.docker.com` convenience
script. No-op when `docker --version` already succeeds.

```yaml
- docker_install: {}
```

### `docker_compose`

Run `docker compose` in a directory containing a `docker-compose.yml`.

```yaml
- name: bring up mysql
  docker_compose:
    dir: /opt/mysql
    state: up                # up | down | restarted | pulled
    pull: true               # docker compose pull before the state action
    wait: true               # only on state: up — append --wait
```

`state: up` runs `docker compose up -d --remove-orphans`. The compose CLI is
idempotent on the docker side; we always report `changed` because parsing
`compose ps` reliably across versions adds complexity we don't need yet.

`wait: true` appends `--wait` to the `up` invocation: the task blocks until
every service with a healthcheck reports `healthy` (or its `start_period`
+ retries expire and compose fails). Use it when a follow-up task needs to
talk to the container — e.g., MySQL: its entrypoint runs `init.sql` during
a temp-server phase and only flips healthy AFTER the real `mysqld` accepts
connections, so `wait: true` is exactly the readiness gate you want. Has
no effect on `down` / `restarted` / `pulled`.

### `docker_build` (CLI-local)

Build an image with `docker buildx` on the CLI host. With `push: true`, push
to a registry afterward. Image is `--load`-ed into the local daemon when
`push: false` (useful for smoke tests).

```yaml
- name: build and push myapi
  docker_build:
    context: .                                     # relative to rulebook
    dockerfile: Dockerfile                         # default
    tag: "registry.example.com/myapi:{{ .git_short_sha }}"
    push: true
    platform: linux/amd64                          # default
    build_args:
      VERSION: "{{ .git_short_sha }}"
```

`tag:` is one tag, `tags:` is a list — provide one or the other, not both.

`docker_build` runs on the CLI host (your laptop / CI), not on the remote
agent. It is therefore independent of host-specific vars and runs once per
`axup` invocation even when fanned out to multiple hosts. The remote pulls
the resulting image via `docker_compose pull` or similar.

### `docker_login` (CLI-local and/or remote)

Authenticate to a Docker registry with creds read from a project-local
encrypted YAML. By default the login is performed BOTH on the CLI (so a
subsequent `docker_build --push` works) and on the remote (so
`docker_compose pull` can fetch private images).

```yaml
- name: log in to private registry
  docker_login:
    registry: registry.example.com
    creds_file: secrets/registry.example.com.yaml   # path relative to rulebook
    # location: both         # both (default) | local | remote
```

The creds file is a YAML with `username:` and `password:` fields and is
typically age-encrypted (see [secrets.md](secrets.md)). The legacy form with
inline `username:` + `password:` / `password_file:` / `password_env:` is also
accepted.

### `mysql_database`

Ensure a MySQL database (and optionally an application user with full
privileges on it) exists. Agent shells out to the `mysql` CLI on the remote
— the client must be installed (e.g. via an upstream `apt: { name: [mysql-client] }`
task). The admin password rides in through the `MYSQL_PWD` env var, never on
argv, so it doesn't show up in `ps`.

```yaml
- name: mysql client
  apt: { name: [mysql-client], update_cache: true }

- name: app db + user
  mysql_database:
    name: myapp
    host: 127.0.0.1
    # port: 3306                             # optional, default 3306
    admin_user: root
    admin_password: "{{ .mysql_root_pw }}"   # load from age-encrypted vars
    # charset: utf8mb4                       # optional, default utf8mb4
    # collation: utf8mb4_0900_ai_ci          # optional, MySQL 8 default
    user: myapp                              # optional — when set, agent also
    password: "{{ .myapp_db_pw }}"           #   does CREATE USER + GRANT ALL
    # user_host: '%'                         # optional grant scope, default '%'
```

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Database name. Restricted to `[A-Za-z_][A-Za-z0-9_]{0,62}` to dodge SQL-quoting / injection. |
| `host` | yes | DB host. No implicit `localhost` / socket fallback — always explicit. |
| `port` | no | TCP port (default 3306). |
| `admin_user` / `admin_password` | yes | Credentials of the account that's allowed to CREATE DATABASE / CREATE USER. |
| `charset` / `collation` | no | Passed to `CREATE DATABASE`. Defaults are `utf8mb4` / `utf8mb4_0900_ai_ci`. |
| `user` / `password` | no | When `user:` is set, the agent additionally runs `CREATE USER` and `GRANT ALL PRIVILEGES ON name.* TO user@user_host`. |
| `user_host` | no | Scope for the user (default `'%'`). Set to `'localhost'` to lock the account to socket / loopback. |

Idempotency: the agent queries `information_schema.SCHEMATA` for the DB and
`mysql.user` for the role before running any CREATE. A converged DB reports
`skipped`. **Charset / collation drift on an existing database is NOT
reconciled** — this task creates, never alters. To change those on an
existing DB, drop down to a `command:` with `ALTER DATABASE`.

Status: `changed` on first run (or when something was missing), `skipped`
once the DB + user both exist, `error` on connection / permission failures.

### `pg_database`

Ensure a PostgreSQL database (and optionally a role with `LOGIN` + full
privileges on it) exists. Agent shells out to `psql`; the admin password
rides in via `PGPASSWORD`. Postgres has no `CREATE DATABASE IF NOT EXISTS`,
so the existence sniff against `pg_database` is load-bearing — without it
a re-run would fail with SQLSTATE 42P04.

```yaml
- name: psql client
  apt: { name: [postgresql-client], update_cache: true }

- name: app db + role
  pg_database:
    name: myapp
    host: 127.0.0.1
    # port: 5432                             # optional, default 5432
    admin_user: postgres
    admin_password: "{{ .pg_admin_pw }}"
    # encoding: UTF8                         # optional, default UTF8
    # owner: myapp                           # optional; defaults to user when set, else admin_user
    user: myapp                              # optional — when set, agent also
    password: "{{ .myapp_db_pw }}"           #   does CREATE ROLE + GRANT ALL
```

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | Database name. Same identifier constraint as MySQL. |
| `host` | yes | DB host (no implicit peer auth). |
| `port` | no | TCP port (default 5432). |
| `admin_user` / `admin_password` | yes | Superuser (or sufficiently-privileged role) that can CREATE DATABASE / CREATE ROLE. |
| `encoding` | no | Encoding for `CREATE DATABASE` (default `UTF8`). |
| `owner` | no | DB owner. Defaults to `user:` when set, else `admin_user`. |
| `user` / `password` | no | When `user:` is set, the agent runs `CREATE ROLE … WITH LOGIN PASSWORD …` and `GRANT ALL PRIVILEGES ON DATABASE name TO user`. |

Idempotency: queries `pg_database` for the DB and `pg_roles` for the role.
Drift on `encoding` / `owner` of an existing DB is NOT reconciled.

Status: `changed` / `skipped` / `error` with the same semantics as
`mysql_database`.

## Reusing tasks across files

There are two ways to split a rulebook and import pieces back together,
both driven by the `use:` task:

- **Local submodules** — separate subdirectories of the same project
  (`mysql/`, `redis/`, …), each with its own phased rulebook. The
  parent splices them at exact positions: `use: ./mysql` (implicit
  phase match) or `use: ./mysql/deploy` (explicit). See
  [local-submodules.md](local-submodules.md).
- **External (git) modules** — versioned, locked, shared across
  projects via `deps:`. Modules use the single-list `tasks:` form;
  the parent references them as `use: <dep>/<module_path>`. See
  [external-rulebooks.md](external-rulebooks.md).

Both forms support per-`use:` `vars:` overrides and rewrite relative
file paths so spliced tasks see their original module dir.

## Module form

A rulebook is in **module form** when it has a top-level `tasks:` field
instead of `bootstrap:` / `deploy:`. Module-form rulebooks are imported via
`use:` from a parent rulebook; they may NOT declare their own `deps:` and
they must use `tasks:`, not the phase split. Module form is only valid
for external (git) modules — local submodules MUST be phased.
See [external-rulebooks.md](external-rulebooks.md) for the import workflow.

```yaml
# example module: common/mysql8/rulebook.yaml in an external dep
name: mysql8
vars:
  port: 3306
  data_dir: /var/lib/mysql
tasks:
  - apt: { name: [mysql-server], update_cache: true }
  - template:
      src: templates/my.cnf.tmpl
      dst: /etc/mysql/my.cnf
  - service: { name: mysql, state: started, enabled: true }
```

## Validation rules

`axup bootstrap` / `axup deploy` reject the rulebook at parse time if:

- `name:` is missing or fails the regex
- `history:` is set outside 0..50
- A task has zero operations or more than one operation
- `mkdir:` is missing `path:`, or has a non-octal `mode:`
- `symlink:` is missing `src:` or `dst:`
- `remove:` is missing `path:`
- `user:` is missing `name:`, has an invalid `state:`, or sets shell/home/groups with `state: absent`
- `group:` is missing `name:` or has an invalid `state:`
- `chmod:` is missing `path:` / `mode:`, or has a non-octal `mode:`
- `chown:` is missing `path:`, or has neither `owner:` nor `group:`
- `download:` is missing `url:` or `dst:`, has a non-octal `mode:`, or has a `sha256:` that isn't 64 hex chars
- `apt:` is missing `name:`, or has an invalid `state:`
- `service:` is missing `name:`, or has an invalid `state:` / `provider:`,
  or sets `enabled:` with `provider: supervisor`
- `docker_compose:` is missing `dir:` or has an invalid `state:`
- `docker_build:` is missing `context:`, has both `tag:` and `tags:`, or
  has neither
- `docker_login:` is missing `registry:`, or sets multiple password sources,
  or sets the inline fields alongside `creds_file:`
- `mysql_database:` / `pg_database:` is missing any of `name` / `host` /
  `admin_user` / `admin_password`, has a `name:` / `user:` / `owner:` that
  doesn't match `[A-Za-z_][A-Za-z0-9_]{0,62}`, has `port` outside 0..65535,
  or sets `user:` without `password:`
- A task has `when_changed:` on a `copy:` or `template:` (those tasks have
  their own sha-based skip logic and the gate would be redundant)
- A `use:` reference points at a dep that isn't declared in `deps:`

## File layout convention

```
my-project/
├── rulebook.yaml
├── axup.lock                  # generated by `axup deps tidy`
├── inventory.yaml               # optional, multi-host
├── recipients.txt               # optional, age public keys
├── templates/
│   ├── compose.yml.tmpl
│   └── supervisor.conf.tmpl
├── files/                       # for `copy:` tasks
│   └── nginx.conf
└── secrets/                     # gitignored by default
    └── registry.com.yaml        # age-encrypted creds
```

Nothing here is mandatory except `rulebook.yaml`; everything else is just the
convention the example projects use.
