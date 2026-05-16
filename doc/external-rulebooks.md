# External rulebooks

A rulebook can pull tasks from another git repository — think Ansible roles,
but with a Go-modules-style lock file and one canonical way to reference
them. The typical use case is "I have a `common/` repo that knows how to set
up mysql, redis, nginx; my service repos pull those modules and add their
own deploy steps on top."

## TL;DR

In the consuming rulebook:

```yaml
name: my-api

deps:
  - name: common
    git: github.com/me/deploy-rules
    version: v1.4.2

bootstrap:
  - use: common/docker
  - use: common/supervisor
  - use: common/mysql8
    vars: { port: 3306, root_password_file: secrets/mysql-root.pw }
```

In `me/deploy-rules` (the external repo):

```
deploy-rules/
├── docker/
│   └── rulebook.yaml
├── supervisor/
│   └── rulebook.yaml
└── mysql8/
    ├── rulebook.yaml
    ├── templates/
    │   └── my.cnf.tmpl
    └── files/
```

Each sub-directory contains a **module rulebook** (a YAML with `name:`,
optional `vars:`, and a single `tasks:` list — not `bootstrap:` / `deploy:`).

## The `deps:` block

```yaml
deps:
  - name: common                                # local handle used in `use:`
    git: github.com/me/deploy-rules             # full repo path
    version: v1.4.2                             # tag, branch, or full sha
```

`git:` accepts several forms:

| Form | Resolves to |
|---|---|
| `github.com/foo/bar` | `https://github.com/foo/bar.git` |
| `git@github.com:foo/bar.git` | as-is (SSH transport) |
| `https://gitlab.com/foo/bar.git` | as-is |
| `file:///path/to/repo` | as-is (useful for tests) |

`version:` may be a tag (`v1.0.0`), a branch (`main`), or a 7-to-40-char hex
SHA. The first time you load a rulebook with `deps:`, the CLI resolves
`version` against the remote with `git ls-remote` to find the actual SHA.

## `axup.lock`

The first run with `deps:` declared writes a `axup.lock` next to the
rulebook:

```yaml
# axup.lock — managed by `axup deps tidy`. Do not edit by hand.
deps:
    - name: common
      git: github.com/me/deploy-rules
      version: v1.4.2
      sha: 9a3b4c…e7d
```

Subsequent runs use the locked SHA exclusively — no network resolution.
Commit this file. To bump versions, edit `version:` in the rulebook, then:

```sh
axup deps tidy             # resolves to a new SHA and rewrites the lock
axup deps verify           # no-network check that the lock matches the rulebook
```

`tidy` is the only operation that talks to the remote; everything else uses
the cached clone.

## Cache layout

Cloned repos live in `$XDG_CACHE_HOME/axup/git/` (`~/Library/Caches/axup`
on macOS, `~/.cache/axup` on Linux), keyed by host, sanitized path, and SHA:

```
~/.cache/axup/git/github.com/me/deploy-rules@9a3b4c…e7d.<urlhash>/
```

Different SHAs of the same repo coexist as sibling directories, so multiple
projects pinned to different versions don't fight over the same clone. The
cache is harmless to delete — `axup` will re-clone on next use.

## `use:` — inlining a module

```yaml
bootstrap:
  - use: common/mysql8                # path inside the dep
    vars:
      port: 3306
      root_password_file: secrets/mysql-root.pw
```

The CLI loads `<dep-clone>/mysql8/rulebook.yaml`, expects it to be a
**module rulebook** (single `tasks:` list), and inlines its tasks at this
position. After expansion the `use:` node is gone from the task tree — every
downstream operation (validation, planning, execution) sees the spliced
tasks as if they had been written in place.

You can use the same module multiple times with different vars; the example
projects do this for the test `common/echo` module.

## Var merging

When tasks from a module are spliced in, their templates are rendered with a
merged var context. Precedence, highest wins:

```
use.vars   >   caller's vars (rulebook + inventory host vars)   >   module defaults
```

`use.vars` values are themselves rendered against the caller's vars BEFORE
the merge — so you can write:

```yaml
vars:
  mysql_port: 3306
bootstrap:
  - use: common/mysql8
    vars:
      port: "{{ .mysql_port }}"
```

…and the module sees `port: 3306` as a concrete value, not the literal
`"{{ .mysql_port }}"`.

For relative file references (`copy.src`, `template.src`, `docker_build.context`,
`docker_login.creds_file`, `password_file`), spliced tasks are rewritten to
**absolute** paths against the module's own directory. So a module's
`template.src: templates/my.cnf.tmpl` continues to point at the right file
even though the host rulebook lives somewhere else entirely.

## Module rulebook format

A module is a rulebook with a single `tasks:` field instead of the
`bootstrap:` / `deploy:` split. Everything else is the same:

```yaml
name: mysql8                   # required; informational

vars:                          # defaults; overridden by use.vars and caller's vars
  port: 3306
  data_dir: /var/lib/mysql

tasks:
  - apt:
      name: [mysql-server, mysql-client]
      update_cache: true

  - template:
      src: templates/my.cnf.tmpl
      dst: /etc/mysql/my.cnf

  - service:
      name: mysql
      state: started
      enabled: true
```

Limitations in the MVP:

- A module rulebook may NOT declare its own `deps:` — transitive deps are
  intentionally not supported. The parent rulebook owns the dep graph.
- A module rulebook may NOT use `bootstrap:` / `deploy:` — only `tasks:`.

These are enforced at parse time with explicit error messages.

## Cycle detection

If two modules `use:` each other (directly or via a longer chain), the
expander detects the cycle and aborts with the file path of the offending
loop:

```
error: use "common/loopy-a": circular use: /…/cache/git/…/loopy-a/rulebook.yaml
```

## `axup deps` commands

```
axup deps tidy         # resolve every dep from its `version:`, rewrite axup.lock
axup deps verify       # check that the rulebook's deps match axup.lock (no network)
```

`tidy` resolves over the network and writes a fresh lock file. `verify` is
offline-friendly and is what you want in CI to catch the
"someone-bumped-version-but-forgot-to-tidy" scenario:

```
DRIFT  common — rulebook=(git=github.com/me/deploy-rules, version=v1.5.0), lock=(git=github.com/me/deploy-rules, version=v1.4.2)
error: 1 dep(s) need `axup deps tidy`
```

A regular `axup bootstrap` / `axup deploy` invocation also auto-resolves
when the lock is missing — `tidy` becomes mandatory only when the lock
exists and disagrees with the rulebook.

## Authentication for private deps

`axup` invokes the user's `git` binary directly, so any auth your shell can
do, the CLI can do: ssh-agent, gitcredential helpers, `~/.netrc`, etc. The
typical pattern for private GitHub/GitLab:

```yaml
deps:
  - name: company-rules
    git: git@github.com:company/deploy-rules.git
    version: v2.0.1
```

…provided `git@github.com` works from the dev machine, that's all you need.

## A worked example

The repository ships [examples/use](../examples/use) which references a
local `file:///tmp/deploy-common-test` repo containing a single `echo`
module. To try it from scratch:

```sh
# build the test deps repo
mkdir -p /tmp/deploy-common-test/echo/templates
cat > /tmp/deploy-common-test/echo/rulebook.yaml <<'EOF'
name: echo
vars:
  greeting: "default greeting"
  target_path: /opt/echo.txt
tasks:
  - template:
      src: templates/greeting.tmpl
      dst: "{{ .target_path }}"
      mode: "0644"
EOF
cat > /tmp/deploy-common-test/echo/templates/greeting.tmpl <<'EOF'
greeting = {{ .greeting | quote }}
EOF
cd /tmp/deploy-common-test
git init -q -b main && git add -A
git -c user.email=t@x -c user.name=t commit -q -m initial
git tag v1.0.0

# resolve and run
cd /path/to/your/project
axup deps tidy --rulebook examples/use/rulebook.yaml
axup bootstrap --host root@your-server --rulebook examples/use/rulebook.yaml
```
