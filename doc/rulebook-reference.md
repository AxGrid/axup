# Rulebook reference

A rulebook is the YAML file (`rulebook.yaml` by convention) that drives a
`axup` invocation. It declares the project's name, top-level variables,
optional external dependencies, optional secrets layout, and two task lists:
`bootstrap:` (called by `axup bootstrap`) and `deploy:` (called by
`axup deploy`).

## Top-level structure

```yaml
name: my-app                          # required; used as the state-dir name on the remote

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

Reserved top-level keys: `name`, `vars`, `deps`, `secrets`, `tasks`
(module-form only). Everything else is a phase. Phase names must match
`^[a-z][a-z0-9_-]*$` and cannot be `status` or `tasks` (CLI-reserved).

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

Every task is one of the following. A task may carry an optional `name:`
(used in CLI output) and, for non-file tasks, `when_changed:` (a list of
remote paths that must have been touched this run for the task to fire).

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

## Module form

A rulebook is in **module form** when it has a top-level `tasks:` field
instead of `bootstrap:` / `deploy:`. Module-form rulebooks are imported via
`use:` from a parent rulebook; they may NOT declare their own `deps:` and
they must use `tasks:`, not the phase split. See
[external-rulebooks.md](external-rulebooks.md) for the import workflow.

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
- A task has zero operations or more than one operation
- `apt:` is missing `name:`, or has an invalid `state:`
- `service:` is missing `name:`, or has an invalid `state:` / `provider:`,
  or sets `enabled:` with `provider: supervisor`
- `docker_compose:` is missing `dir:` or has an invalid `state:`
- `docker_build:` is missing `context:`, has both `tag:` and `tags:`, or
  has neither
- `docker_login:` is missing `registry:`, or sets multiple password sources,
  or sets the inline fields alongside `creds_file:`
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
