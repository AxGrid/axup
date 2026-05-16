// sops.go — in-process structural encryption for YAML / JSON / INI /
// dotenv via the official sops Go library (github.com/getsops/sops/v3).
// No runtime dep on the `sops` binary — everything links into the axup
// CLI directly.
//
// Whole-file age (the original axup behavior) stays for everything
// that isn't a structured format: binaries, .conf, .pem, certs, …
// Dispatch lives at the caller side (cli/secrets.go for the CLI, and
// the unified `Decrypt(data)` byte-sniff for the runtime path).
package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	sopslib "github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/keys"
	"github.com/getsops/sops/v3/stores/dotenv"
	"github.com/getsops/sops/v3/stores/ini"
	"github.com/getsops/sops/v3/stores/json"
	"github.com/getsops/sops/v3/stores/yaml"
	"github.com/getsops/sops/v3/version"
)

// store is the minimal subset of sops's Store interface we use — keeps
// our type signature unchanged when sops bumps the upstream interface.
type store interface {
	LoadPlainFile(in []byte) (sopslib.TreeBranches, error)
	LoadEncryptedFile(in []byte) (sopslib.Tree, error)
	EmitEncryptedFile(in sopslib.Tree) ([]byte, error)
	EmitPlainFile(in sopslib.TreeBranches) ([]byte, error)
}

// SopsFormat returns the sops format string ("yaml", "json", "ini",
// "dotenv") that `path`'s extension maps to, or "" if the path isn't
// a structured format axup encrypts in sops-style.
func SopsFormat(path string) string {
	base := filepath.Base(path)
	for _, suffix := range []string{".tmpl", ".template", ".j2"} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	switch strings.ToLower(filepath.Ext(base)) {
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	case ".ini", ".toml": // sops's ini store also parses toml
		return "ini"
	case ".env":
		return "dotenv"
	}
	return ""
}

// sniffSopsFormat picks a format for a runtime-only byte slice (used by
// Decrypt() when it sees the sops marker but doesn't know the path).
// Each sops format embeds its metadata block in a format-specific way,
// so look for distinguishing keys:
//
//   dotenv → `sops_version=` (flat key=value)
//   ini    → `[sops]` section header
//   json   → leading `{`
//   yaml   → everything else (the default and most common)
//
// CLI commands should still pass the format explicitly when the path
// is known (extension is the source of truth); this sniff is the
// fallback for handlers that only see bytes.
func sniffSopsFormat(data []byte) string {
	if bytes.Contains(data, []byte("sops_version=")) {
		return "dotenv"
	}
	if bytes.Contains(data, []byte("\n[sops]")) || bytes.HasPrefix(data, []byte("[sops]")) {
		return "ini"
	}
	trim := bytes.TrimLeft(data, "\r\n\t ")
	if len(trim) > 0 && trim[0] == '{' {
		return "json"
	}
	return "yaml"
}

func storeForFormat(format string) store {
	switch format {
	case "json":
		return &json.Store{}
	case "ini":
		return &ini.Store{}
	case "dotenv":
		return &dotenv.Store{}
	default:
		return &yaml.Store{}
	}
}

// AgeRecipientLines filters a recipients.txt body down to age1...
// pubkey lines. sops's age integration doesn't accept ssh-* recipients
// directly (those would need ssh-to-age conversion — out of scope for
// the initial integration). Comments and blanks are skipped.
func AgeRecipientLines(recipientsBody []byte) []string {
	var out []string
	for _, line := range strings.Split(string(recipientsBody), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "age1xxxx some-comment" → "age1xxxx"
		if i := strings.IndexAny(line, " \t"); i > 0 {
			line = line[:i]
		}
		if strings.HasPrefix(line, "age1") {
			out = append(out, line)
		}
	}
	return out
}

// SopsEncryptBytes wraps a plaintext file body in sops-style structural
// encryption against the given age recipients. Returns the encrypted
// file bytes, ready to write back to disk.
func SopsEncryptBytes(plaintext []byte, format string, ageRecipients []string) ([]byte, error) {
	if len(ageRecipients) == 0 {
		return nil, fmt.Errorf("sops encrypt: no age1... recipients available (sops doesn't accept ssh-* recipients directly; add an age key to recipients.txt)")
	}
	store := storeForFormat(format)
	branches, err := store.LoadPlainFile(plaintext)
	if err != nil {
		return nil, fmt.Errorf("sops parse %s: %w", format, err)
	}
	masterKeys, err := buildMasterKeys(ageRecipients)
	if err != nil {
		return nil, err
	}
	tree := sopslib.Tree{
		Branches: branches,
		Metadata: sopslib.Metadata{
			KeyGroups: []sopslib.KeyGroup{masterKeys},
			Version:   version.Version,
		},
	}
	dataKey, errs := tree.GenerateDataKey()
	if err := joinErrors(errs); err != nil {
		return nil, fmt.Errorf("sops generate data key: %w", err)
	}
	cipher := aes.NewCipher()
	// Inline common.EncryptTree so we don't import cmd/sops/common
	// (it pulls the kms / azkv / vault sub-packages — ~30 MB of SDKs).
	unencryptedMac, err := tree.Encrypt(dataKey, cipher)
	if err != nil {
		return nil, fmt.Errorf("sops encrypt tree: %w", err)
	}
	tree.Metadata.LastModified = time.Now().UTC()
	tree.Metadata.MessageAuthenticationCode, err = cipher.Encrypt(unencryptedMac, dataKey, tree.Metadata.LastModified.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("sops encrypt MAC: %w", err)
	}
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		return nil, fmt.Errorf("sops emit %s: %w", format, err)
	}
	return out, nil
}

// SopsDecryptBytes is the runtime decrypt path for sops-encrypted
// data. It reuses axup's identity loader (loadIdentities) and feeds
// the discovered age secret keys to sops via the SOPS_AGE_KEY env var
// for the duration of the call.
//
// Format is sniffed from the data when not known; pass an explicit
// format when the caller does know (e.g. CLI knows the source path).
//
// Inline implementation (rather than decrypt.Data) so we avoid the
// cmd/sops/common dependency tree (KMS / Vault / GCS / AWS SDKs).
func SopsDecryptBytes(data []byte, format string) ([]byte, error) {
	if format == "" {
		format = sniffSopsFormat(data)
	}
	cleanup, err := setSopsAgeEnv()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	store := storeForFormat(format)
	tree, err := store.LoadEncryptedFile(data)
	if err != nil {
		return nil, fmt.Errorf("sops parse encrypted %s: %w", format, err)
	}
	dataKey, err := decryptDataKey(&tree)
	if err != nil {
		return nil, fmt.Errorf("sops decrypt data key: %w\n\n%s\n\nFixes:\n  - point at a recipient key: --age-key /path/to/key.txt (or $AXUP_AGE_KEY)\n  - or re-encrypt with your public key added to recipients.txt:\n      axup secrets encrypt", err, IdentityHints())
	}
	cipher := aes.NewCipher()
	if _, err := tree.Decrypt(dataKey, cipher); err != nil {
		return nil, fmt.Errorf("sops decrypt tree: %w", err)
	}
	out, err := store.EmitPlainFile(tree.Branches)
	if err != nil {
		return nil, fmt.Errorf("sops emit plain %s: %w", format, err)
	}
	return out, nil
}

// decryptDataKey walks the tree's KeyGroups until some MasterKey is
// able to unwrap the data key. For axup that's always an age key —
// other key types only fire if a user's sops file was originally
// encrypted with KMS/Vault/etc, in which case we report a clear error.
func decryptDataKey(tree *sopslib.Tree) ([]byte, error) {
	var lastErr error
	for _, group := range tree.Metadata.KeyGroups {
		for _, mk := range group {
			k, err := mk.Decrypt()
			if err != nil {
				lastErr = err
				continue
			}
			return k, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("no key could decrypt (last error: %w)", lastErr)
	}
	return nil, errors.New("no keys in sops metadata")
}

// buildMasterKeys wraps a slice of age recipient strings into the
// MasterKey list sops needs for a KeyGroup. Returns an error if any
// recipient fails to parse — better to bail than silently lose a key.
func buildMasterKeys(recipients []string) ([]keys.MasterKey, error) {
	out := make([]keys.MasterKey, 0, len(recipients))
	for _, r := range recipients {
		mk, err := sopsage.MasterKeyFromRecipient(r)
		if err != nil {
			return nil, fmt.Errorf("sops parse age recipient %q: %w", r, err)
		}
		out = append(out, mk)
	}
	return out, nil
}

// setSopsAgeEnv stringifies axup's loaded age identities and shoves
// them into SOPS_AGE_KEY (newline-separated, what the sops library
// expects). Returns a cleanup that restores the original env var so
// concurrent callers and the test suite don't bleed into each other.
//
// SSH-key identities (loaded for whole-file age) are skipped — sops's
// age backend wants Bench32 AGE-SECRET-KEY-… secret keys, not SSH.
func setSopsAgeEnv() (func(), error) {
	ids, src, err := loadIdentities()
	if err != nil {
		return func() {}, err
	}
	var secretKeys []string
	for _, id := range ids {
		if x, ok := id.(*age.X25519Identity); ok {
			secretKeys = append(secretKeys, x.String())
		}
	}
	if len(secretKeys) == 0 {
		hint := src
		if hint == "" {
			hint = "(none)"
		}
		return func() {}, fmt.Errorf("no age1... secret keys available for sops decrypt — loaded identities from %s, but sops accepts only AGE-SECRET-KEY-… (SSH keys not supported here).\n\n%s", hint, IdentityHints())
	}
	prev, hadPrev := os.LookupEnv(sopsage.SopsAgeKeyEnv)
	if err := os.Setenv(sopsage.SopsAgeKeyEnv, strings.Join(secretKeys, "\n")); err != nil {
		return func() {}, err
	}
	return func() {
		if hadPrev {
			_ = os.Setenv(sopsage.SopsAgeKeyEnv, prev)
		} else {
			_ = os.Unsetenv(sopsage.SopsAgeKeyEnv)
		}
	}, nil
}

// joinErrors collapses sops's [...]error-style returns into one error.
func joinErrors(errs []error) error {
	var nonNil []error
	for _, e := range errs {
		if e != nil {
			nonNil = append(nonNil, e)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	if len(nonNil) == 1 {
		return nonNil[0]
	}
	return errors.Join(nonNil...)
}
