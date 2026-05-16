# CLI reference

Every subcommand, flag, and environment variable in one place.

## Synopsis

```
axup <command> [flags] [args]
```

Top-level commands:

| Command | Purpose |
|---|---|
| `axup bootstrap` | Run the rulebook's `bootstrap:` tasks on one or more hosts |
| `axup deploy` | Run the rulebook's `deploy:` tasks on one or more hosts |
| `axup run <phase>` | Run any named phase (incl. custom phases like `deploy_crash`, `migrate`) |
| `axup logs <svc>...` | Tail logs of one or more services declared in `services:` |
| `axup status` | Report each host's recorded state and any drift, read-only |
| `axup init` | Scaffold a stub `rulebook.yaml` in the current directory |
| `axup deps tidy` | Resolve `deps:` to fresh SHAs and rewrite `axup.lock` |
| `axup deps verify` | Check that `axup.lock` matches the declared deps |
| `axup secrets encrypt` | Age-encrypt a file (or every declared file) |
| `axup secrets decrypt` | Print a decrypted file to stdout |
| `axup secrets edit` | Decrypt → `$EDITOR` → encrypt cycle |
| `axup secrets status` | Report encrypted / plaintext / missing state of declared secret files |
| `axup version` | Print the binary version |
| `axup help [command]` | Help for a specific command |

## Persistent flags

These work on every subcommand:

| Flag | Effect |
|---|---|
| `--key PATH` | Use a specific SSH private key. Overrides ssh-agent and `~/.ssh/id_*` discovery. |
| `--password STR` | SSH password. Visible in `ps` — prefer `--ask-password`. |
| `--ask-password` | Prompt for SSH password on a TTY. Mutex with `--password`. |
| `--sudo` | Wrap the agent in `sudo -H -S`. Assumes NOPASSWD unless a sudo password flag is set. |
| `--sudo-password STR` | Sudo password. Implies `--sudo`. Visible in `ps` — prefer `--ask-sudo-password`. |
| `--ask-sudo-password` | Prompt for sudo password on a TTY. Implies `--sudo`. Mutex with `--sudo-password`. |
| `--check` | Dry-run: handlers report `would_change` instead of applying. State is not rewritten. |
| `--dry-run` | Alias for `--check`. |
| `--no-color` | Disable ANSI colors. Auto-disabled when stdout is not a TTY. |
| `--age-key PATH` | Path to age identity file. **Repeatable** for multiple keys. Overrides auto-discovery. |
| `-h, --help` | Help for the command. |

### Auth flags

The three SSH auth flags are independent — pick none, one, or several:

- **None given**: `axup` tries the ssh-agent socket (`$SSH_AUTH_SOCK`) first,
  then walks `~/.ssh/id_ed25519`, `id_rsa`, `id_ecdsa` for unencrypted private
  keys.
- **`--key PATH`** is set: ONLY that key is tried. Auto-discovery is skipped.
- **`--password STR`** or **`--ask-password`** is set: password auth is offered
  in addition to any explicit key.

Sudo flags are orthogonal to SSH auth. Typical real-world combos:

| Use case | Flags |
|---|---|
| Root over SSH (default) | `--host root@addr` (no extra flags) |
| Non-root user, NOPASSWD sudo | `--host user@addr --sudo` |
| Non-root user, interactive sudo | `--host user@addr --ask-sudo-password` |
| CI with non-default SSH key | `--key /etc/deploy/ci_id --sudo --sudo-password "$SUDO_PW"` |
| Password-auth Linux box | `--ask-password --ask-sudo-password --host user@addr` |

### Color and TTY

The CLI emits ANSI escape sequences for status marks (✓ green, · gray,
≈ yellow, ⚠ yellow, ⊘ red, ✗ red) and the host tag prefix (cyan). Colors are
auto-disabled when:

- stdout is not a terminal (e.g., piped to a file or captured in `$()`)
- the `NO_COLOR` environment variable is set to any non-empty value
- `--no-color` is passed

## `axup bootstrap` / `axup deploy`

```
axup bootstrap [flags]
axup deploy [flags]
```

Local flags (same for both):

| Flag | Default | Effect |
|---|---|---|
| `--host STR` | — | Target host: `user@addr[:port]` literal OR an inventory host name. |
| `--group STR` | — | Inventory group name. Mutex with `--host`. |
| `--rulebook PATH` | `rulebook.yaml` | Path to the rulebook YAML. |
| `--vars PATH` | — | Optional YAML file with extra vars (merged after rulebook defaults and git auto-vars; per-host inventory still wins). Relative paths resolve against CWD, falling back to the rulebook's directory. |

Exactly one of `--host` or `--group` is required. See
[inventory-multi-host.md](inventory-multi-host.md) for how each resolves.

### Examples

```sh
# Single ad-hoc host
axup bootstrap --host root@1.2.3.4

# By inventory name
axup deploy --host prod-1

# Whole group, parallel
axup deploy --group prod

# Preview only
axup bootstrap --check --host root@1.2.3.4

# Custom key + sudo with prompted password
axup deploy --key ~/.ssh/deploy_rsa --ask-sudo-password --host deploybot@server

# Different rulebook
axup deploy --rulebook ./prod/rulebook.yaml --host prod-1
```

## `axup run`

```
axup run <phase> [flags]
```

Runs any phase from the rulebook against the chosen host(s). A rulebook
can declare arbitrary top-level keys alongside `bootstrap:` / `deploy:`
— each is a phase, runnable with `axup run`:

```yaml
# rulebook.yaml
bootstrap:    [...]
deploy:       [...]
deploy_crash: [...]
migrate:      [...]
```

```sh
axup run deploy_crash --group stage         # only push the game modules
axup run migrate --host prod-1              # run migrations on one host
axup run axup --check --group prod        # equivalent to `axup deploy --check`
```

Phase names must match `^[a-z][a-z0-9_-]*$` and cannot be `status` /
`tasks` / `services` / `logs` (CLI-reserved). All phases of one
rulebook share `~/.axup-state/<name>/state.json` on the remote — a
file written by `bootstrap` is "in sync" when a later `deploy_crash`
references the same path.

Local flags: same as `axup bootstrap` / `axup deploy`
(`--host`, `--group`, `--rulebook`, `--vars`).

## `axup logs`

```
axup logs <service> [<service>...] [flags]
axup logs --list [flags]
```

Tails files declared in the rulebook's `services:` block over SSH, in
parallel across multiple hosts. Each line is prefixed with `[host]` so
fan-out output stays attributable.

Local flags:

| Flag | Default | Effect |
|---|---|---|
| `--host STR` | — | Single host (inventory alias or `user@addr[:port]`). |
| `--group STR` | — | Inventory group — every host in parallel. Mutex with `--host`. |
| `--rulebook PATH` | `rulebook.yaml` | Path to the rulebook YAML. |
| `--vars PATH` | — | Optional vars file (log paths get templated through it). |
| `-n, --tail N` | `20` | Initial lines per file before streaming starts. |
| `--no-follow` | `false` | Snapshot mode — print and exit; no `tail -F`. |
| `--list` | `false` | Print declared services + paths and exit. No SSH. |

`axup logs` invokes `tail -n N -q [-F] <path1> <path2> …` in one SSH
session per host. `-q` suppresses the per-file `==> path <==` header
that tail emits when multiple paths are tailed together — service
attribution comes from the log content itself (slog's `service=` field
in JSON output, etc).

```sh
# Snapshot of the last 200 lines of crash, no streaming
axup logs crash --host stage-1 -n 200 --no-follow

# Stream billing + supervisord master log on every prod host
axup logs billing supervisor --group prod

# Discover what's declared
axup logs --list
```

Ctrl-C closes every SSH session and the corresponding remote `tail`
exits cleanly.

## `axup status`

Read-only mode. Connects to each host, asks the agent to read
`~/.axup-state/<rulebook>/state.json`, and reports each tracked file's
status:

| Status | Meaning |
|---|---|
| `in_sync` | File on disk matches the recorded sha256 + mode |
| `drift` | File exists but content or mode has diverged from what was deployed |
| `missing` | State knows the file, but it's no longer on disk |

```sh
axup status --host root@server
axup status --group prod
```

Same flags as `bootstrap` / `axup` (minus phase-related ones). State is
never rewritten in this mode.

## `axup init`

```
axup init
```

Writes a stub `rulebook.yaml` in the current directory. Fails if the file
already exists.

## `axup deps tidy` / `axup deps verify`

```
axup deps tidy [--rulebook PATH]
axup deps verify [--rulebook PATH]
```

`tidy` resolves every entry in `deps:` against the remote via `git ls-remote`,
clones each into the cache, and writes a fresh `axup.lock` next to the
rulebook.

`verify` does not touch the network: it just compares the rulebook's `deps:`
to the lock and reports drift. Useful as a CI guardrail.

See [external-rulebooks.md](external-rulebooks.md) for the full workflow.

## `axup secrets …`

```
axup secrets encrypt [FILE] [--rulebook PATH]
axup secrets decrypt FILE   [--rulebook PATH]
axup secrets edit    FILE   [--rulebook PATH]
axup secrets status         [--rulebook PATH]
```

| Subcommand | Behavior |
|---|---|
| `encrypt FILE` | Encrypt that single file in place against `recipients.txt`. |
| `encrypt` (no arg) | Encrypt every file in `rulebook.yaml`'s `secrets.files`. Skips already-encrypted files. Exits non-zero if a declared file is missing. |
| `decrypt FILE` | Print plaintext to stdout. |
| `edit FILE` | Decrypt → `$EDITOR` → encrypt back in place. Works on a non-existent file too. |
| `status` | Report each `secrets.files` entry as `encrypted` / `PLAINTEXT` / `MISSING`. Exit non-zero if anything's not encrypted. |

`--rulebook` locates `recipients.txt` and reads the `secrets:` block.

Identity for decryption is resolved by the same flag / env / discovery chain
as `axup bootstrap` — see the `--age-key` row above and
[secrets.md](secrets.md) for the full story.

## Environment variables

| Var | Effect |
|---|---|
| `AXUP_AGE_KEY` | Colon-separated list of age identity files (alternative to `--age-key`). |
| `EDITOR` | Editor used by `axup secrets edit`. Defaults to `vi`. |
| `NO_COLOR` | When set (any non-empty value), disables ANSI colors. |
| `SSH_AUTH_SOCK` | Standard SSH agent socket; honored when no `--key` is given. |

## Files the CLI reads from your home

| Path | When |
|---|---|
| `~/.ssh/known_hosts` | Host key verification; auto-falls back to insecure-ignore if missing |
| `~/.ssh/id_ed25519`, `id_rsa`, `id_ecdsa` | SSH auth fallback when ssh-agent / `--key` are absent |
| `~/.config/age/keys.txt` | Age identity fallback when `--age-key` / `$AXUP_AGE_KEY` are absent |

## Files the CLI manages in your project

| Path | When |
|---|---|
| `rulebook.yaml` | Always |
| `axup.lock` | Created/updated by `axup deps tidy`; auto-written when deps are present but no lock exists |
| `inventory.yaml` | Optional; auto-detected next to `rulebook.yaml` |
| `recipients.txt` (or what `secrets.recipients_file` points to) | Required for `secrets encrypt` / `secrets edit` |
| `secrets/*.yaml` | The encrypted secret payloads referenced from `creds_file:` / `password_file:` |

## Files the agent manages on each remote host

| Path | Purpose |
|---|---|
| `/tmp/axupd-<rand>` | Uploaded agent binary; removed at end of every run |
| `~/.axup-state/<rulebook-name>/state.json` | sha256 + mode of every managed file (under root's home when sudo is used) |

## Wire protocol

For curious users — the JSON envelope on the SSH session's stdin/stdout:

CLI → agent (single object, terminated by EOF):

```json
{
  "rulebook_name": "myapi",
  "phase": "bootstrap",
  "dry_run": false,
  "status_only": false,
  "tasks": [
    {"id": "bootstrap.1", "name": "install supervisor", "type": "apt",
     "apt_packages": ["supervisor"], "apt_state": "present"},
    ...
  ]
}
```

Agent → CLI (newline-delimited):

```json
{"type": "task_start", "task_id": "bootstrap.1", "message": "install supervisor"}
{"type": "task_end",   "task_id": "bootstrap.1", "status": "changed",
 "stdout": "Setting up supervisor (4.2.5-1ubuntu0.1) ...", "message": "installed 1 package(s): supervisor"}
{"type": "done"}
```

Status values: `ok`, `changed`, `skipped`, `error`, `would_change` (dry-run),
`in_sync` / `drift` / `missing` (status mode).
