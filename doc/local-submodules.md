# Local submodules

When a project's rulebook starts juggling 4 things (database, cache, app,
proxy), one flat `rulebook.yaml` becomes a 300-line wall. `use: ./<path>`
lets you split each component into its own directory — with its own
phases, vars, and config files — and splice them back into the main
rulebook at exactly the position and phase you choose.

For `use:` against an external git repository (versioned, locked,
shared across projects), see [external-rulebooks.md](external-rulebooks.md).
The mechanism is the same; only the reference syntax differs.

## What it looks like

```
myproject/
├── rulebook.yaml                # orchestrator
├── mysql/
│   ├── rulebook.yaml            # mysql's own bootstrap + deploy phases
│   └── templates/
│       └── my.cnf.tmpl
└── redis/
    └── rulebook.yaml
```

**`mysql/rulebook.yaml`** — phased, just like the parent:

```yaml
name: mysql-component
vars:
  port: 3306                                  # component-local defaults
  data_dir: /var/lib/mysql
bootstrap:
  - apt: { name: [mysql-server] }
  - template:
      src: templates/my.cnf.tmpl              # relative paths Just Work —
      dst: /etc/mysql/my.cnf                  # they're rewritten at splice time
  - service: { name: mysql, state: started, enabled: true }
deploy:
  - template: { src: templates/my.cnf.tmpl, dst: /etc/mysql/my.cnf }
  - service: { name: mysql, state: reloaded }
    when_changed: [/etc/mysql/my.cnf]
```

**`rulebook.yaml`** (parent) — picks where each component's tasks land:

```yaml
name: myapp
vars:
  port: 3307                                  # overrides mysql's default

bootstrap:
  - apt: { name: [build-essential] }          # 1. parent's own setup
  - use: ./mysql                              # 2. mysql's bootstrap spliced here
  - use: ./redis/bootstrap                    # 3. redis's bootstrap (explicit phase)
  - copy: { src: files/myapp, dst: /opt/myapp }   # 4. parent's app

deploy:
  - use: ./mysql                              # mysql's *deploy* phase (auto-matched)
  - copy: { src: files/myapp, dst: /opt/myapp }
  - service: { name: myapp, state: restarted }
```

`axup bootstrap` produces this task sequence on each host:

```
apt build-essential
mysql.apt mysql-server
mysql.template /etc/mysql/my.cnf
mysql.service mysql
redis.…           (whatever redis/bootstrap declares)
copy /opt/myapp
```

## Reference syntax

| Form | Resolves to |
|---|---|
| `use: ./mysql` | The parent's currently-running phase, inside `./mysql/rulebook.yaml`. E.g. when the parent's `deploy:` phase is running, this picks `mysql/rulebook.yaml`'s `deploy:`. |
| `use: ./mysql/bootstrap` | Explicitly the `bootstrap:` phase of `./mysql/rulebook.yaml`, regardless of which parent phase is running. |
| `use: ./mysql/deploy_crash` | Same, for a custom phase. |
| `use: ../shared/mysql` | Relative paths walk normally — `..`, nested dirs all work. |
| `use: /opt/rules/common/redis` | Absolute paths resolve as-is (rare; useful for CI-mounted shared rule dirs). |

Disambiguation between dir-with-implicit-phase and explicit-phase form is
filesystem-driven: if the whole ref points at a directory containing
`rulebook.yaml`, it's implicit. Otherwise the last segment is treated as
the phase name and the prefix as the dir.

Asking for a phase that doesn't exist in the submodule is an error
(silent skip would hide typos). The error message lists the phases that
ARE declared so you can pick one or fix the spelling:

```
error: phase "deploy": use "./redis": submodule "redis-component" at
  /myproject/redis has no phase "deploy" (available: [bootstrap])
```

## Vars precedence

When a submodule declares its own `vars:`, those are component defaults.
The parent rulebook's vars override them, and any inline `vars:` on the
`use:` line overrides everything for that one splice:

```yaml
# parent
vars:
  port: 3307                              # overrides mysql's 3306

bootstrap:
  - use: ./mysql                          # uses port=3307
  - use: ./mysql/deploy                   # also uses port=3307
    vars:
      port: 3308                          # …but THIS one uses 3308
```

Effective precedence (highest wins):

```
inline `use.vars` > parent rulebook vars > submodule's own vars
```

Inventory host vars and `--vars` file still sit ABOVE the parent's
rulebook vars (see [rulebook-reference.md](rulebook-reference.md#external-vars---vars)).

Inline `use.vars` strings are pre-rendered against the parent's vars,
so you can forward parent values into a submodule with templating:

```yaml
bootstrap:
  - use: ./mysql
    vars:
      port: "{{ .mysql_port }}"           # parent's mysql_port → submodule's port
```

## What gets spliced (and what doesn't)

In the MVP, only the picked phase's `tasks:` list is imported. The
submodule's other top-level fields are ignored:

| Submodule field | Behavior in parent |
|---|---|
| `vars:` | merged into the splice context (see precedence above) |
| `bootstrap:` / `deploy:` / custom phases | only the one selected by the `use:` ref is spliced; siblings are inert |
| `tasks:` (module-form) | **error** — local submodules must be phased. For module-form reuse, declare it as an external git dep. |
| `services:` | ignored. If `axup logs mysql` should work, put the catalog entry in the parent. |
| `secrets:` | ignored. Manage secret files from the parent. |
| `history:` | ignored. Parent's `history: N` wins. |
| `deps:` | **error** — submodules can't have their own git deps (transitive resolution is out of scope). |
| `name:` | required, but only used in error messages. |

Templates and files in the submodule (e.g. `mysql/templates/my.cnf.tmpl`)
work seamlessly — their relative `src:` paths are rewritten to be
absolute against the submodule's directory before the splice.

## Cycles

`use: ./mysql/bootstrap → use: ./redis/bootstrap → use: ./mysql/bootstrap`
is detected and reported:

```
error: phase "bootstrap": circular use: /myproject/mysql#bootstrap
```

The cycle key is `(submodule_dir, phase)` so a submodule's bootstrap can
legitimately call another submodule's deploy without triggering the
guard, and a parent can splice the same submodule's bootstrap into
multiple phases (e.g. `bootstrap:` and a custom `reapply:` phase both
include `use: ./mysql/bootstrap`).

## When to pick local vs external

**Local** when the components are coupled to THIS project and shouldn't
be versioned independently. They're co-edited with the parent rulebook
in the same commit and live in the same git repo.

**External (git deps)** when the components are reusable across projects
and benefit from independent versioning. Use a `deps:` entry with `git:`
and `version:`; the lock file pins exact SHAs. See
[external-rulebooks.md](external-rulebooks.md).

You can mix both freely in one rulebook:

```yaml
deps:
  - { name: common, git: github.com/me/common-rules, version: v1.4.2 }

bootstrap:
  - use: common/mysql8                          # external module
  - use: ./redis                                # local submodule
```
