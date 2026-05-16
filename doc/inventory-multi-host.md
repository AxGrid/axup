# Inventory and multi-host

For one-off runs, `axup bootstrap --host root@1.2.3.4` is enough. When you
have a fleet — even of two — declare them once in `inventory.yaml` and
address them by name.

## File location

`inventory.yaml` lives next to `rulebook.yaml`. It's optional; the CLI looks
for it on every run and silently falls back to the `user@addr` form if the
file isn't present.

## Schema

```yaml
hosts:
  prod-1:
    address: 1.2.3.4           # required: IP or DNS
    user: root                 # optional; default "root"
    port: 22                   # optional; default "22"
    vars:                      # optional; merged into the rulebook's vars
      env: prod
      app_port: 8080
  prod-2:
    address: 5.6.7.8
    user: deployer
    vars: { env: prod, app_port: 8080 }
  stage:
    address: 10.0.0.1
    vars: { env: stage, app_port: 9080 }

groups:
  prod: [prod-1, prod-2]       # list of host names
  all:  [prod-1, prod-2, stage]
```

Validation: every group member must be a declared host; missing addresses
fail at parse time with a clear message.

## Resolving `--host` and `--group`

The CLI resolves the identifier as follows:

1. `--host user@addr[:port]` (literal containing `@`) — used as an ad-hoc
   spec; the inventory isn't consulted. The output tag is the literal spec.
2. `--host <name>` — looked up in `inventory.hosts`. If absent, error with
   the list of declared host names.
3. `--group <name>` — looked up in `inventory.groups`. Errors on unknown
   group or empty group.
4. `--host` and `--group` are mutually exclusive; one is required.

## Per-host vars

Each host's `vars:` block is merged into `rb.Vars` LAST, after rulebook
defaults and git auto-vars. So precedence (highest wins):

```
inventory host vars   >   git auto-vars   >   rulebook vars defaults
```

A typical use-case: same rulebook, different `env` or DB host per server:

```yaml
# rulebook.yaml
vars:
  env: unknown                                # default; overridden per host
  db_host: localhost
deploy:
  - template:
      src: templates/app.env.tmpl
      dst: /opt/myapi/.env
```

```yaml
# inventory.yaml
hosts:
  prod-1:
    address: 10.1.1.10
    vars: { env: prod,  db_host: db.prod.internal }
  stage-1:
    address: 10.2.1.10
    vars: { env: stage, db_host: db.stage.internal }
```

```
axup deploy --group all
```

Each host receives a `.env` rendered with its own `env` and `db_host`.

## Parallel execution

`--group` runs hosts in parallel. Under the hood `axup` uses
`golang.org/x/sync/errgroup`: each host gets its own goroutine, its own SSH
session, its own `/tmp/axupd-<rand>`, its own remote state. The first error
cancels the group — but goroutines that are already inside `RunAgent` finish
their current task before they stop.

Concurrent output is serialized through a mutex so lines don't tear, and each
line is prefixed with `[host-name]` so you can follow per-host progress
visually:

```
[prod-1] arch=amd64
[stage-1] arch=amd64
[prod-1] agent uploaded …
[stage-1] agent uploaded …
[prod-1] ▶ render app config (deploy.1)
[stage-1] ▶ render app config (deploy.1)
[prod-1]   ✓ status=changed path=/opt/myapi/.env
[stage-1]   ✓ status=changed path=/opt/myapi/.env
[prod-1] done
[prod-1] summary: changed=1
[stage-1] done
[stage-1] summary: changed=1
```

## Local-task behavior in multi-host runs

CLI-local tasks (`docker_build`, `docker_login` with `location: local`) run
**once per `axup` invocation**, using the FIRST host's vars. They are
assumed to be host-invariant — the typical workflow is "build one image, push
once, every host pulls the same tag". If you need a genuinely different build
per host, run `axup deploy --host <h>` separately for each.

Remote tasks (everything else) fan out — each host gets its own Plan built
from its own merged var context.

## A note on shared state files

The agent's state file lives at `~/.axup-state/<rulebook_name>/state.json`
on the remote. If two logical inventory hosts point at the SAME physical
server (e.g., during local testing), they share a single state file. Writing
DIFFERENT paths is fine — both end up as separate keys. Writing the SAME path
from two hosts concurrently is a race; the final state may only reflect one
of them. Real deployments use distinct servers and don't hit this.

## Mixing `--host` and ad-hoc spec

The same `inventory.yaml` is used regardless of how you name the target.
That means you can keep using a literal `user@addr` once in a while (for
debugging or one-off boxes) without touching the inventory:

```
# uses inventory.hosts["prod-1"].user / .address / .vars
axup deploy --host prod-1

# bypasses inventory entirely — rulebook vars are the only vars
axup deploy --host root@new-box.example.com
```
