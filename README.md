<p align="center">
  <img src="assets/logo.svg" width="160" alt="deploy"/>
</p>

<h1 align="center">deploy</h1>

<p align="center">
  Ansible-style server provisioning and project rollout, in a single Go binary.<br/>
  Works natively on macOS, talks plain SSH, ships with an embedded remote agent.
</p>

---

## What it is

`deploy` reads a YAML rulebook, opens an SSH connection to your server(s), uploads a small embedded agent into `/tmp`, and streams a plan of tasks to it: install packages, render templates, write configs, manage services, bring up Docker Compose stacks, build and push images, and so on. State (sha256 hashes of every managed file) lives on the remote, so re-runs only touch what actually changed.

Compared to Ansible: no Python on either side, no roles/handlers ceremony, two verbs (`bootstrap` and `deploy`), and everything that isn't a task is just one of nine well-defined task types.

## Quick start

```sh
# 1. Build (cross-compiles the agent for linux/amd64+arm64 and embeds it into the CLI)
make

# 2. Scaffold a rulebook
mkdir myproject && cd myproject
/path/to/deploy/bin/deploy init

# 3. Edit rulebook.yaml, then:
deploy bootstrap --host root@1.2.3.4
deploy deploy    --host root@1.2.3.4
```

A 5-minute end-to-end walkthrough lives in [doc/getting-started.md](doc/getting-started.md).

## Highlights

- **Nine task primitives**: `command`, `copy`, `template`, `apt`, `service` (systemd + supervisor), `docker_install`, `docker_build`, `docker_compose`, `docker_login`
- **Remote state with sha256 diffs**: re-applying a rulebook only touches files whose content or mode actually changed; out-of-band drift is detected
- **`deploy status`**: read-only mode that reports `in_sync` / `drift` / `missing` for every tracked file on every host
- **`deploy bootstrap --check`** (dry-run): see what would change without applying anything
- **External rulebook modules**: declare `deps:` with `{git, version}`, lock to SHAs, import sub-rulebooks via `use: common/mysql8`
- **Inventory + multi-host**: optional `inventory.yaml` with named hosts, groups, and per-host vars; `--group prod` fans out in parallel via errgroup
- **age-encrypted secrets**: public keys committed to the repo via `recipients.txt`, transparent decrypt at deploy time, multiple identities supported (per-developer keys, SSH keys, `~/.config/age/keys.txt`)
- **Auth modes**: SSH key (auto-discover or `--key`), SSH password (`--password` / `--ask-password`), sudo with or without password (`--sudo` / `--sudo-password` / `--ask-sudo-password`)
- **Docker pipeline**: build images locally with `docker buildx`, push to your registry with `docker_login` creds, pull on the remote via `docker_compose pull` — all chained from one rulebook
- **Colored output** with per-host summaries and a content-detecting plain/encrypted file path — `NO_COLOR` and `--no-color` honored
- **Git auto-vars**: `git_sha`, `git_short_sha`, `git_branch`, `git_dirty` are injected into the template context automatically — handy for `tag: "myapi:{{ .git_short_sha }}"`

## Documentation

| Doc | What it covers |
|---|---|
| [Getting started](doc/getting-started.md) | Build, scaffold, first bootstrap and deploy in ~5 minutes |
| [Rulebook reference](doc/rulebook-reference.md) | Every task type and field, with copy-paste-ready YAML |
| [Inventory & multi-host](doc/inventory-multi-host.md) | `inventory.yaml`, `--host` vs `--group`, per-host vars, parallel runs |
| [Secrets (age)](doc/secrets.md) | Recipients, encryption, identity discovery, multi-developer setup |
| [External rulebooks](doc/external-rulebooks.md) | `deps:`, `use:`, `deploy.lock`, module layout, var merge precedence |
| [CLI reference](doc/cli-reference.md) | Every command, every flag, every env var |

## Example projects

The repository ships five end-to-end examples, each runnable against a real Linux box you have SSH access to:

| Example | Demonstrates |
|---|---|
| [examples/simple](examples/simple) | `copy` + `template` + `when_changed` |
| [examples/server](examples/server) | `apt` + `docker_install` + `service` + `docker_compose` + supervisor-managed process |
| [examples/build](examples/build) | Local `docker_build --push` + remote `docker_compose pull` with encrypted registry creds |
| [examples/use](examples/use) | External rulebook module loaded via `deps:` + `use: common/echo` |
| [examples/multihost](examples/multihost) | `inventory.yaml`, two logical hosts, parallel `--group all` |

## Architecture in one paragraph

The CLI is a single Go binary (`cmd/deploy`). Inside it lives the cross-compiled `deployd` agent for `linux/amd64` and `linux/arm64`, embedded via `//go:embed`. On every run the CLI uploads the agent into `/tmp/deployd-<rand>` via `ssh ... 'cat > path && chmod +x'`, pipes a JSON `Plan` to its stdin, and reads newline-delimited `Event` JSON from its stdout. The agent maintains `~/.deploy-state/<rulebook>/state.json` for idempotency and gets removed from `/tmp` at the end. Local-only tasks (`docker_build`, `docker_login` for the CLI side) run before SSH opens. See [doc/cli-reference.md](doc/cli-reference.md#wire-protocol) for the wire details.

## License

MIT.
