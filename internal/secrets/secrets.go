// Package secrets implements transparent age encryption for files referenced
// by docker_login.creds_file / password_file. The rulebook layer calls
// Decrypt on every loaded blob; plaintext passes through unchanged, ciphertext
// (age armor) is decrypted with auto-discovered identities.
//
// Identity discovery order (first match wins):
//  1. The `--age-key PATH` flag (sets the package-level KeyPath var)
//  2. $DEPLOY_AGE_KEY env var (same effect)
//  3. ~/.config/age/keys.txt   (the canonical age identity file)
//  4. ~/.ssh/id_ed25519        (SSH key as age identity, unencrypted only)
//  5. ~/.ssh/id_rsa            (same)
//
// Encryption is text-armored so the resulting files are git-friendly diffs
// even if a reviewer looks at them.
package secrets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
)

// KeyPaths, when non-empty, overrides identity auto-discovery. Set by the CLI
// from the (repeatable) --age-key flag during PersistentPreRun. Each path is
// parsed independently; the resulting identities are all tried during Decrypt
// so a project with N keys (one per developer) lets any of them decrypt.
var KeyPaths []string

// armorHeader is the first line of an age text-armored ciphertext. We use this
// to decide whether Decrypt should do anything at all — plaintext passes
// through untouched, so secrets stay opt-in.
const armorHeader = "-----BEGIN AGE ENCRYPTED FILE-----"

// sopsMarker shows up in any sops-encrypted YAML/JSON/INI/ENV/TOML file:
// every encrypted leaf value is wrapped as ENC[AES256_GCM,data:...,iv:...].
// Cheaper than parsing the format just to find the `sops:` metadata block.
var sopsMarker = []byte("ENC[AES256_GCM,")

// LooksEncrypted reports whether data is encrypted by either backend
// (age whole-file or sops structural). Drives `Decrypt`'s dispatch.
func LooksEncrypted(data []byte) bool {
	return LooksAgeEncrypted(data) || LooksSopsEncrypted(data)
}

// LooksAgeEncrypted reports whether data starts with the age armor
// header (i.e. produced by axup's whole-file age path).
func LooksAgeEncrypted(data []byte) bool {
	trim := bytes.TrimLeft(data, "\r\n\t ")
	return bytes.HasPrefix(trim, []byte(armorHeader))
}

// LooksSopsEncrypted reports whether data contains a sops-style
// encrypted leaf (`ENC[AES256_GCM,...`). One match is enough — sops
// puts the marker on every encrypted value plus the metadata block.
func LooksSopsEncrypted(data []byte) bool {
	return bytes.Contains(data, sopsMarker)
}

// IsStructuredPath reports whether `path` should be encrypted in
// sops-style (per-value, structure visible in PRs) rather than
// whole-file age. Decision is purely by extension: .yaml / .yml /
// .json / .ini / .env / .toml. Trailing template suffixes (.tmpl,
// .template, .j2) are stripped so `inventory.prod.yaml.tmpl` still
// routes to sops.
func IsStructuredPath(path string) bool {
	base := filepath.Base(path)
	for _, suffix := range []string{".tmpl", ".template", ".j2"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".yaml", ".yml", ".json", ".ini", ".env", ".toml":
		return true
	}
	return false
}

// Decrypt returns plaintext for either age- or sops-encrypted input.
// Pass-through if neither marker matches. Dispatches by content
// sniff so callers don't need to track the original path.
//
// Order matters: check the age armor header FIRST. Old-style whole-file
// age yaml/json/ini files still on disk (from before the dual-backend
// release) must keep working — their extension hints at sops but the
// header is unambiguous proof of age. Only if the header is absent do
// we look at the sops marker; this also avoids ever passing age bytes
// to sops's parser, which produces a useless "no key could decrypt"
// error instead of an honest "age decrypt failed".
func Decrypt(data []byte) ([]byte, error) {
	if LooksAgeEncrypted(data) {
		return decryptAge(data)
	}
	if LooksSopsEncrypted(data) {
		return SopsDecryptBytes(data, "")
	}
	return data, nil
}

// decryptAge is the whole-file age path, split out so Decrypt's
// dispatch stays a clean three-branch read.
func decryptAge(data []byte) ([]byte, error) {
	ids, src, err := loadIdentities()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("file is age-encrypted but no decryption identity is available — pass --age-key PATH, set $AXUP_AGE_KEY, or put a key at ~/.config/age/keys.txt")
	}
	r := armor.NewReader(bytes.NewReader(data))
	dec, err := age.Decrypt(r, ids...)
	if err != nil {
		return nil, fmt.Errorf("age decrypt (tried identity from %s): %w", src, err)
	}
	out, err := io.ReadAll(dec)
	if err != nil {
		return nil, fmt.Errorf("age decrypt: read: %w", err)
	}
	return out, nil
}

// Encrypt armors `data` against `recipients`. Used by `deploy secrets encrypt`.
func Encrypt(data []byte, recipients []age.Recipient) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients — populate recipients.txt with age or ssh public keys")
	}
	var buf bytes.Buffer
	armorW := armor.NewWriter(&buf)
	w, err := age.Encrypt(armorW, recipients...)
	if err != nil {
		return nil, fmt.Errorf("age encrypt: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("age write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("age close: %w", err)
	}
	if err := armorW.Close(); err != nil {
		return nil, fmt.Errorf("armor close: %w", err)
	}
	return buf.Bytes(), nil
}

// loadIdentities aggregates identities across every source so a deploy run can
// be authenticated by ANY of the configured keys — typical use case is a
// shared CI host loaded with several team members' keys, or a single
// developer whose recipients.txt also includes peers (encrypt-side multi-key,
// decrypt-side multi-key).
//
// When --age-key is set (one or more times) only those paths are consulted;
// auto-discovery is skipped. When $AXUP_AGE_KEY (or its pre-rename alias
// $DEPLOY_AGE_KEY) is set (colon-separated path list, like $PATH) only those
// paths are consulted. Otherwise we accumulate from ~/.config/age/keys.txt +
// ~/.ssh/id_*.
func loadIdentities() ([]age.Identity, string, error) {
	if len(KeyPaths) > 0 {
		return loadFromPaths(KeyPaths, "--age-key")
	}
	envName, env := "AXUP_AGE_KEY", os.Getenv("AXUP_AGE_KEY")
	if env == "" {
		envName, env = "DEPLOY_AGE_KEY", os.Getenv("DEPLOY_AGE_KEY")
	}
	if env != "" {
		var paths []string
		for _, p := range strings.Split(env, string(os.PathListSeparator)) {
			p = strings.TrimSpace(p)
			if p != "" {
				paths = append(paths, p)
			}
		}
		return loadFromPaths(paths, "$"+envName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, "", err
	}
	var ids []age.Identity
	var sources []string
	// Native age identity file.
	cfgPath := filepath.Join(home, ".config", "age", "keys.txt")
	if subs, _, err := tryParseIdentityFile(cfgPath); err == nil && len(subs) > 0 {
		ids = append(ids, subs...)
		sources = append(sources, cfgPath)
	}
	// SSH keys at canonical names. Each is parsed independently; passphrase-
	// protected keys are skipped silently.
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		p := filepath.Join(home, ".ssh", name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		id, err := agessh.ParseIdentity(data)
		if err != nil {
			continue
		}
		ids = append(ids, id)
		sources = append(sources, p)
	}
	return ids, strings.Join(sources, ", "), nil
}

func loadFromPaths(paths []string, label string) ([]age.Identity, string, error) {
	var ids []age.Identity
	var sources []string
	for _, p := range paths {
		subs, _, err := tryParseIdentityFile(p)
		if err != nil {
			return nil, p, fmt.Errorf("%s %s: %w", label, p, err)
		}
		ids = append(ids, subs...)
		sources = append(sources, p)
	}
	return ids, label + " " + strings.Join(sources, ","), nil
}

// tryParseIdentityFile is like parseIdentityFile but treats "file missing" as
// (nil, "", nil) rather than an error — useful for auto-discovery where the
// canonical paths may not exist.
func tryParseIdentityFile(path string) ([]age.Identity, string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, "", nil
	}
	ids, err := parseIdentityFile(path)
	return ids, path, err
}

// parseIdentityFile reads an age identity file. The file format is:
// - blank lines and lines starting with "#" are ignored
// - everything else is parsed by age.ParseIdentities (X25519, scrypt)
// We also try to fall back to SSH-format keys if the file looks like one.
func parseIdentityFile(path string) ([]age.Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read identity file %s: %w", path, err)
	}
	// Native age identity?
	if ids, err := age.ParseIdentities(bytes.NewReader(data)); err == nil && len(ids) > 0 {
		return ids, nil
	}
	// SSH key?
	if id, err := agessh.ParseIdentity(data); err == nil {
		return []age.Identity{id}, nil
	}
	return nil, fmt.Errorf("identity file %s: unrecognized format (not an age key or supported SSH key)", path)
}

// RecipientsPath resolves where recipients live for a given rulebook dir and
// an optional explicit name (from rulebook.Secrets.RecipientsFile). When the
// explicit name is empty, we look for recipients.txt then .axup/recipients.txt
// (.deploy/recipients.txt kept as a backwards-compat fallback).
// Returns the chosen path or "" if no candidate exists.
func RecipientsPath(rulebookDir, explicit string) string {
	if explicit != "" {
		p := explicit
		if !filepath.IsAbs(p) {
			p = filepath.Join(rulebookDir, explicit)
		}
		return p
	}
	for _, p := range []string{
		filepath.Join(rulebookDir, "recipients.txt"),
		filepath.Join(rulebookDir, ".axup", "recipients.txt"),
		filepath.Join(rulebookDir, ".deploy", "recipients.txt"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// LoadRecipients parses a `recipients.txt` file: one age public key (age1…)
// or SSH public key (ssh-ed25519/ssh-rsa…) per line. Lines starting with `#`
// are comments. Empty lines ignored. Returns a friendly error if the file is
// absent or empty.
// LoadAgeRecipientStrings is the sops-mode counterpart of LoadRecipients:
// reads the same recipients file but returns the raw `age1...` strings
// (filtering out comments, blanks, and SSH-pubkey lines that sops can't
// consume). Used by the CLI when encrypting a sops-format file.
func LoadAgeRecipientStrings(path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("no recipients file — set secrets.recipients_file in rulebook.yaml or create recipients.txt next to it")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	out := AgeRecipientLines(data)
	if len(out) == 0 {
		return nil, fmt.Errorf("recipients file %s has no age1... lines (sops doesn't accept ssh-* recipients)", path)
	}
	return out, nil
}

func LoadRecipients(path string) ([]age.Recipient, error) {
	if path == "" {
		return nil, fmt.Errorf("no recipients file — set secrets.recipients_file in rulebook.yaml or create recipients.txt next to it")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var recs []age.Recipient
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := parseRecipientLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, i+1, err)
		}
		recs = append(recs, r)
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("%s contains no recipients", path)
	}
	return recs, nil
}

// snippet truncates a recipient-line preview to keep the error one line.
// Recipients are normally 60–600 chars; 32 is enough to spot a typo
// (stray `4#`, `// comment`, blank-but-not-empty whitespace, …).
func snippet(s string) string {
	const max = 32
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func parseRecipientLine(line string) (age.Recipient, error) {
	switch {
	case strings.HasPrefix(line, "age1"):
		return age.ParseX25519Recipient(line)
	case strings.HasPrefix(line, "ssh-"):
		return agessh.ParseRecipient(line)
	default:
		return nil, fmt.Errorf("unrecognized recipient format (expected age1… or ssh-…, got %q) — if this is a comment, prefix with `#`", snippet(line))
	}
}
