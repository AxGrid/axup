# Secrets

The `axup` CLI encrypts per-project secret files (registry passwords,
private keys, anything you don't want committed in plaintext) using
[age](https://age-encryption.org/). Public keys live in the repo;
decryption happens transparently at deploy time from whichever identity the
runner can find.

This page covers: setup, the two pieces of config a project needs, the CLI
commands, and how multiple developers share a project.

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
`.deploy/recipients.txt`.

### 2. `secrets:` block in `rulebook.yaml` — which files are encrypted

```yaml
secrets:
  recipients_file: recipients.txt        # default; relative to rulebook dir
  files:
    - secrets/registry.example.com.yaml
    - secrets/db.pw
```

The block is optional. With it, `axup secrets encrypt` (no args) operates
on the whole declared set, and `axup secrets status` reports the state of
every file (encrypted / plaintext / missing). Without it, you encrypt files
one at a time by passing each path to `axup secrets encrypt FILE`.

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
