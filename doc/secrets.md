# Secrets

The `axup` CLI encrypts per-project secret files (registry passwords,
private keys, anything you don't want committed in plaintext) using
[age](https://age-encryption.org/). Public keys live in the repo;
decryption happens transparently at deploy time from whichever identity the
runner can find.

This page covers: setup, the two pieces of config a project needs, the CLI
commands, and how multiple developers share a project.

## Two encryption modes (chosen automatically by extension)

axup picks the encryption format from the file's extension:

| File extension | Mode | What the encrypted file looks like |
|---|---|---|
| `.yaml` / `.yml` / `.json` / `.ini` / `.toml` / `.env` | **sops-style structural** — leaf values encrypted, keys remain visible | git-diffable; reviewers can see which key changed without decrypting |
| anything else (binaries, `.conf`, `.pem`, `.cert`, …) | **whole-file age armor** | opaque base64 blob between `-----BEGIN AGE ENCRYPTED FILE-----` markers |

Trailing template suffixes (`.tmpl`, `.template`, `.j2`) are stripped
before the extension lookup — `inventory.prod.yaml.tmpl` still routes
to the YAML/sops path.

Both modes use age recipients from the same `recipients.txt` (sops's
SSH-key recipients aren't supported — only `age1…` lines pass through;
the rest are silently dropped when running sops-mode encrypt).

Decryption is auto-detected from file content (`-----BEGIN AGE…` →
age path; `ENC[AES256_GCM,…` anywhere in the body → sops path), so
callers don't need to know which mode a file was encrypted in.

## What encryption protects

Two task fields go through the encryption pipeline:

- `docker_login.creds_file` — a YAML with `{username, password}`
- `docker_login.password_file` and any other inline `password_file`

When the rulebook is loaded, these files are read and run through
`secrets.Decrypt`. Plaintext passes through unchanged; age ciphertext is
decrypted with the available identity. There is no flag to "enable
encryption" — it's content-detection based by the
`-----BEGIN AGE ENCRYPTED FILE-----` armor header.

## Two pieces of config

### 1. `recipients.txt` — who can decrypt

A plain text file next to `rulebook.yaml`, committed to git. Each line is one
public key, in either age (`age1…`) or SSH (`ssh-ed25519`, `ssh-rsa`, …)
format. Comments start with `#`; blank lines are ignored.

```
# Public keys allowed to decrypt secrets/*.yaml
age1qxv8ph2…                                # alice (age key)
ssh-ed25519 AAAAC3Nz… alice@laptop          # alice (SSH pubkey)
ssh-rsa AAAAB3Nz… bob@laptop                # bob
age1tgg7g…                                  # ci runner
```

A file is encrypted to ALL recipients listed here — any single matching
private key is enough to decrypt.

Override the filename via `secrets.recipients_file:` in the rulebook (see
below). The default search path is `recipients.txt`, then
`.axup/recipients.txt` (with `.deploy/recipients.txt` kept as a backwards-
compat fallback).

### 2. `secrets:` block in `rulebook.yaml` — which files are encrypted

```yaml
secrets:
  recipients_file: recipients.txt        # default; relative to rulebook dir
  files:
    - secrets/registry.example.com.yaml
    - secrets/db.pw
    # Long form: per-file recipients override AND/OR explicit backend.
    - path: secrets/prod-creds.yaml
      recipients: recipients.prod.txt
    - path: secrets/ssh-only.yaml         # ssh-* recipients only? force age
      format: age                         # so sops's "no age1… recipients" doesn't bite
```

The block is optional. With it, `axup secrets encrypt` (no args) operates
on the whole declared set, and `axup secrets status` reports the state of
every file (encrypted / plaintext / missing). Without it, you encrypt files
one at a time by passing each path to `axup secrets encrypt FILE`.

Each `files:` entry is either a bare string (just a path) or a mapping with:

| Field | Meaning |
|---|---|
| `path` (required) | File path, relative to rulebook dir. |
| `recipients` | Override `recipients_file` for this file only (e.g. stage vs prod keyrings in one rulebook). |
| `format` | `""` / `auto` (default) → pick by extension. `age` → force whole-file age (use when recipients are ssh-\* keys, since sops only accepts age1…). `sops` → force sops (only valid for `.yaml/.yml/.json/.ini/.env/.toml`). |
| `decrypted_to` | Switches the entry to **pair mode**. `path` is the committed encrypted file; `decrypted_to` is the plaintext working copy that `axup secrets unseal` materialises and `axup secrets seal` re-encrypts. `axup secrets unseal` also appends `decrypted_to` to the nearest `.gitignore` automatically. |

### Pair mode (encrypted-at-rest + plaintext working copy)

Two-file workflow for inventories and configs you want to **edit as plaintext**
but **commit as encrypted**:

```yaml
secrets:
  files:
    - path: inventory.prod.enc.yaml         # committed, encrypted
      decrypted_to: inventory.prod.yaml     # gitignored, plaintext
      recipients: recipients.prod.txt
      format: age
```

Usage:

```sh
axup secrets unseal      # decrypts every .enc → plaintext, adds to .gitignore
$EDITOR inventory.prod.yaml
axup secrets seal        # re-encrypts plaintext → .enc, ready to commit
git add inventory.prod.enc.yaml && git commit
```

Pair entries are skipped by `encrypt` / `decrypt FILE` (with a hint pointing
at `seal` / `unseal`) — those verbs are for in-place entries (no
`decrypted_to`). `secrets status` reports both `path` (encrypted/missing) and
`decrypted_to` (plaintext/not-on-disk) for each pair entry.

The auto-`.gitignore` block looks like this and is idempotent across runs:

```
# Plaintext working copies of axup secrets — never commit.
# (axup secrets unseal manages this block.)
inventory.prod.yaml
inventory.stage.yaml
```

## CLI commands

### Generate or import an identity

```
age-keygen -o ~/.config/age/keys.txt        # generate a fresh age keypair
age-keygen -y ~/.config/age/keys.txt        # print the public key
```

If you'd rather reuse your existing SSH key, skip `age-keygen`. The CLI will
parse `~/.ssh/id_ed25519` / `id_rsa` / `id_ecdsa` as an age identity provided
the SSH key is NOT passphrase-protected.

### `axup secrets encrypt`

```
# encrypt every file declared in secrets.files
axup secrets encrypt --rulebook rulebook.yaml

# encrypt a specific file
axup secrets encrypt secrets/db.pw
```

The output is age text-armored. Files that are already encrypted are skipped
with a `skip` line; missing declared files cause exit 1 so a CI hook can flag
them.

### `axup secrets decrypt`

```
axup secrets decrypt secrets/registry.com.yaml
```

Prints plaintext to stdout. Useful for piping into `cat`, `jq`, or grepping
without leaving plaintext on disk.

### `axup secrets edit`

```
axup secrets edit secrets/registry.com.yaml
```

Decrypts the file into a temp path, opens it with `$EDITOR` (defaulting to
`vi`), then encrypts the edited content back in place. Works on
non-existent files too — useful for creating fresh encrypted content.

### `axup secrets status`

```
axup secrets status --rulebook rulebook.yaml
```

Reports the on-disk state of every file declared in `secrets.files`:

```
rulebook:    rulebook.yaml
recipients:  /home/dev/project/recipients.txt

  encrypted  secrets/registry.com.yaml
  PLAINTEXT  secrets/db.pw
  MISSING    secrets/legacy.token

3 file(s) not in the expected encrypted state
```

Exits non-zero if anything is plaintext or missing — wire it into your
pre-commit hook.

### Pre-commit guard (recommended)

`axup secrets encrypt` is best-effort: if recipients parsing fails for one
file, the others still get encrypted, but the failing file STAYS PLAINTEXT
on disk. A `git add .` followed by `git commit` would leak it.

Two-layer defence:

1. **Read the FAIL/WARNING lines** after every `axup secrets encrypt` — it
   prints a "STILL PLAINTEXT" block listing exactly which files need fixing.
2. **Wire `axup secrets status` into git's pre-commit hook** so the commit
   is blocked outright if anything declared in `secrets.files` is plaintext
   or missing:

   ```sh
   # .git/hooks/pre-commit (chmod +x)
   #!/bin/sh
   set -e
   axup secrets status --rulebook deploy/rulebook.yaml
   ```

   Or via [pre-commit.com](https://pre-commit.com/), `.pre-commit-config.yaml`:

   ```yaml
   - repo: local
     hooks:
       - id: axup-secrets-status
         name: axup secrets status
         entry: axup secrets status --rulebook deploy/rulebook.yaml
         language: system
         pass_filenames: false
   ```

## Identity discovery (decryption)

`axup` finds the private key for decryption in this order. Each branch is
"exclusive" — when it matches it doesn't fall through to the next:

1. `--age-key PATH` flag — **repeatable**, so multiple keys can be specified
   in a single command:
   ```
   axup deploy --age-key ~/keys/alice.key --age-key ~/keys/bob.key --host root@server
   ```
2. `$AXUP_AGE_KEY` environment variable — colon-separated path list,
   like `$PATH`:
   ```
   AXUP_AGE_KEY="/keys/alice.key:/keys/bob.key" axup deploy --host …
   ```
3. Auto-discovery (when none of the above is set) — accumulates from BOTH:
   - `~/.config/age/keys.txt` (which can itself contain multiple identities)
   - `~/.ssh/id_ed25519`, `~/.ssh/id_rsa`, `~/.ssh/id_ecdsa`

In every branch, ALL identities found are tried against the ciphertext;
whichever one matches wins. So a CI host with several developers' keys, or a
laptop with both an age key and an SSH key, just works without extra config.

## Multiple developers

The typical workflow for adding a new team member:

```sh
# On the new dev's machine
age-keygen -o ~/.config/age/keys.txt
age-keygen -y ~/.config/age/keys.txt
# → prints age1newdev…

# Then a dev who already has decrypt access:
echo "age1newdev… # alice@new-laptop" >> recipients.txt
axup secrets encrypt          # re-encrypts every file in secrets.files to the new recipient set
git add recipients.txt secrets/
git commit -m "add alice"

# alice pulls, runs `axup` — her auto-discovery finds ~/.config/age/keys.txt
```

Removing a developer is symmetric: delete their line from `recipients.txt`,
`axup secrets encrypt` to rotate, commit. Note that historical commits
still contain ciphertext their key can decrypt — if you need to revoke access
to a specific value, rotate the underlying secret (e.g., generate a new
registry token) rather than relying on rewriting history.

## Recommended `.gitignore`

The bundled `.gitignore` includes `secrets/` by default — which protects you
from accidentally committing plaintext. Once a file in there is encrypted,
either remove that line (and rely on `secrets status` as a guardrail) or
adopt a convention of encrypted files living somewhere outside `secrets/`
(e.g., `enc/` or `vault/`).

## Caveats

- **Passphrase-protected SSH keys are not supported** — the agent calls
  `agessh.ParseIdentity` directly, which rejects encrypted PEM. Either
  generate an age key, remove the passphrase from your SSH key, or use a
  separate unencrypted key dedicated to deploy.
- **The age library used is `filippo.io/age` v1.3.x** — pure Go, no
  external `age` binary needed at deploy time. (You'll want the
  `age-keygen` binary on your dev machine to mint keys, though.)
- **State files (`.axup-state/.../state.json`) are NOT encrypted** —
  they live on the remote and contain sha256 digests and modes, not the
  plaintext content of any managed file. If you consider the file paths
  themselves sensitive, lock down the remote `/root` accordingly.
- **Git history retains ciphertext** — rotating recipients re-encrypts the
  current file but old commits still contain old ciphertext. The fix is to
  rotate the secret value itself, not to rewrite git.
