package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/spf13/cobra"

	"github.com/axgrid/axup/internal/rulebook"
	"github.com/axgrid/axup/internal/secrets"
)

var secretsRulebook string

var secretsCmd = &cobra.Command{
	Use:   "secrets",
	Short: "Encrypt / decrypt / edit per-project secret files (age)",
	Long: `Encrypts files in-place against the public keys listed in recipients.txt
next to the rulebook. The optional secrets: block in rulebook.yaml
lists which files are encrypted and lets `+"`encrypt`"+` and `+"`status`"+`
operate on the whole set.

Decryption uses the auto-discovered identity (--age-key, $AXUP_AGE_KEY,
~/.config/age/keys.txt, or ~/.ssh/id_*).`,
}

var secretsEncryptCmd = &cobra.Command{
	Use:   "encrypt [FILE]",
	Short: "Encrypt FILE in place; with no arg, encrypt every file in secrets.files",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		rb, err := loadRulebookHeader(secretsRulebook)
		if err != nil {
			return err
		}
		entries, err := resolveSecretFiles(rb, args)
		if err != nil {
			return err
		}

		// Cache parsed recipients lists by file path so we only read each
		// recipients.txt once across a large secrets.files batch. Two
		// flavors: parsed age.Recipient (whole-file age) and raw
		// age1... strings (sops-format files).
		recCache := map[string][]age.Recipient{}
		ageStrCache := map[string][]string{}

		// Collect per-entry failures instead of returning on the first
		// error: if one file's recipients have a typo, we still want
		// the user to see which OTHER files are now plaintext on disk
		// (so they don't accidentally commit them while fixing the
		// typo). Each entry's error is logged inline; the final
		// warning lists every plaintext file that needs attention.
		encrypted, skipped, missing, failed := 0, 0, 0, 0
		var stillPlaintext []string
		var lastErr error
		for _, e := range entries {
			// Pair entries (with decrypted_to:) belong to the seal/unseal
			// workflow. Encrypt is for in-place entries only — point the
			// user at the right verb rather than silently doing the wrong
			// thing to the encrypted file at `path`.
			if e.entry.IsPair() {
				fmt.Printf("skip    %s (pair entry — use `axup secrets seal` to encrypt %s → %s)\n",
					e.absPath, e.entry.DecryptedTo, e.entry.Path)
				skipped++
				continue
			}
			data, err := os.ReadFile(e.absPath)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Printf("MISSING %s (declared but not on disk)\n", e.absPath)
					missing++
					continue
				}
				fmt.Fprintf(os.Stderr, "FAIL    %s: read: %v\n", e.absPath, err)
				failed++
				lastErr = err
				continue
			}
			if secrets.LooksEncrypted(data) {
				fmt.Printf("skip    %s (already encrypted)\n", e.absPath)
				skipped++
				continue
			}
			// Dispatch by `format:` override if set, otherwise by file
			// extension: sops-style structural for yaml/json/ini/env/toml
			// (keeps the keys visible in diffs), whole-file age for
			// everything else. `format: age` is the escape hatch for
			// projects whose recipients are ssh-* keys (which sops can't
			// consume) but whose files are yaml/json/…
			var (
				out  []byte
				mode string
			)
			useSops, sopsFormat, err := chooseFormat(e)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", e.absPath, err)
				failed++
				stillPlaintext = append(stillPlaintext, e.absPath)
				lastErr = err
				continue
			}
			if useSops {
				ageStrs, err := ageStringsForEntry(rb, e, ageStrCache)
				if err != nil {
					fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", e.absPath, err)
					failed++
					stillPlaintext = append(stillPlaintext, e.absPath)
					lastErr = err
					continue
				}
				out, err = secrets.SopsEncryptBytes(data, sopsFormat, ageStrs)
				if err != nil {
					fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", e.absPath, err)
					failed++
					stillPlaintext = append(stillPlaintext, e.absPath)
					lastErr = err
					continue
				}
				mode = "sops/" + sopsFormat
			} else {
				recs, err := recsForEntry(rb, e, recCache)
				if err != nil {
					fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", e.absPath, err)
					failed++
					stillPlaintext = append(stillPlaintext, e.absPath)
					lastErr = err
					continue
				}
				out, err = secrets.Encrypt(data, recs)
				if err != nil {
					fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", e.absPath, err)
					failed++
					stillPlaintext = append(stillPlaintext, e.absPath)
					lastErr = err
					continue
				}
				mode = "age"
			}
			if err := atomicWrite(e.absPath, out, 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "FAIL    %s: write: %v\n", e.absPath, err)
				failed++
				stillPlaintext = append(stillPlaintext, e.absPath)
				lastErr = err
				continue
			}
			fmt.Printf("OK      %s  (%s, recipients=%s)\n", e.absPath, mode, e.recipientsLabel)
			encrypted++
		}
		fmt.Printf("\nencrypted=%d skipped=%d missing=%d failed=%d\n", encrypted, skipped, missing, failed)
		if len(stillPlaintext) > 0 {
			fmt.Fprintln(os.Stderr, "\n⚠ WARNING: the following file(s) are STILL PLAINTEXT on disk")
			fmt.Fprintln(os.Stderr, "  and will leak secrets if committed to git:")
			for _, p := range stillPlaintext {
				fmt.Fprintf(os.Stderr, "    %s\n", p)
			}
			fmt.Fprintln(os.Stderr, "  Fix the error(s) above and re-run `axup secrets encrypt`.")
			fmt.Fprintln(os.Stderr, "  Wire `axup secrets status` into pre-commit to catch this automatically.")
		}
		switch {
		case failed > 0:
			return fmt.Errorf("%d file(s) failed to encrypt (last error: %w)", failed, lastErr)
		case missing > 0:
			return fmt.Errorf("%d declared file(s) were missing on disk", missing)
		}
		return nil
	},
}

var secretsDecryptCmd = &cobra.Command{
	Use:   "decrypt FILE",
	Short: "Print decrypted contents of FILE to stdout (for inspection)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := args[0]
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		// We know the path here, so pass the sops format explicitly
		// when applicable. (The runtime Decrypt() byte-sniff path can
		// only choose yaml-vs-json; dotenv/ini need the extension.)
		var out []byte
		if format := secrets.SopsFormat(path); format != "" && secrets.LooksSopsEncrypted(data) {
			out, err = secrets.SopsDecryptBytes(data, format)
		} else {
			out, err = secrets.Decrypt(data)
		}
		if err != nil {
			return err
		}
		_, _ = os.Stdout.Write(out)
		return nil
	},
}

var secretsEditCmd = &cobra.Command{
	Use:   "edit FILE",
	Short: "Decrypt FILE into $EDITOR, then re-encrypt in place",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		path := args[0]
		rb, err := loadRulebookHeader(secretsRulebook)
		if err != nil {
			return err
		}
		// Resolve format via the secrets.files entry (if FILE is declared),
		// falling back to extension-based dispatch otherwise.
		useSops, sopsFormat, err := chooseFormatForPath(rb, path)
		if err != nil {
			return err
		}

		// Pick the per-file recipients override if the user is editing a file
		// that's declared in secrets.files; otherwise fall back to the default.
		// We only need one or the other depending on the mode.
		var (
			recs    []age.Recipient
			ageStrs []string
		)
		if useSops {
			ageStrs, err = ageStringsForPath(rb, path)
		} else {
			recs, err = recipientsForPath(rb, path)
		}
		if err != nil {
			return err
		}
		// Read + decrypt (or accept plain if file doesn't exist yet).
		// For sops formats we MUST pass the format explicitly — the
		// runtime byte-sniff only resolves yaml-vs-json.
		var plain []byte
		if data, err := os.ReadFile(path); err == nil {
			if useSops && secrets.LooksSopsEncrypted(data) {
				plain, err = secrets.SopsDecryptBytes(data, sopsFormat)
			} else {
				plain, err = secrets.Decrypt(data)
			}
			if err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}

		// Write plaintext to a tmp file with restrictive perms so the editor
		// can open it.
		tmp, err := os.CreateTemp("", "deploy-secret-*.yaml")
		if err != nil {
			return err
		}
		tmpPath := tmp.Name()
		defer func() { _ = os.Remove(tmpPath) }()
		if err := os.Chmod(tmpPath, 0o600); err != nil {
			return err
		}
		if _, err := tmp.Write(plain); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}

		ed := exec.Command("/bin/sh", "-c", editor+" "+shellQuote(tmpPath))
		ed.Stdin = os.Stdin
		ed.Stdout = os.Stdout
		ed.Stderr = os.Stderr
		if err := ed.Run(); err != nil {
			return fmt.Errorf("editor exited non-zero: %w", err)
		}

		edited, err := os.ReadFile(tmpPath)
		if err != nil {
			return err
		}
		var out []byte
		if useSops {
			out, err = secrets.SopsEncryptBytes(edited, sopsFormat, ageStrs)
		} else {
			out, err = secrets.Encrypt(edited, recs)
		}
		if err != nil {
			return err
		}
		if err := atomicWrite(path, out, 0o600); err != nil {
			return err
		}
		fmt.Printf("saved %s\n", path)
		return nil
	},
}

// secretsSealCmd walks pair entries and encrypts each `decrypted_to`
// (plaintext, gitignored working copy) into `path` (committed encrypted
// file). Non-pair entries are skipped — they belong to `encrypt`.
var secretsSealCmd = &cobra.Command{
	Use:   "seal",
	Short: "Encrypt every pair entry's `decrypted_to` (plaintext) → `path` (encrypted)",
	Long: `For every entry in secrets.files that has a decrypted_to: field, read the
plaintext working copy, encrypt it with the entry's recipients, and write
the result to the committed path. Mirrors ` + "`unseal`" + `.

Entries without decrypted_to: are skipped — use ` + "`encrypt`" + ` for those.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		rb, err := loadRulebookHeader(secretsRulebook)
		if err != nil {
			return err
		}
		if rb.Secrets == nil || len(rb.Secrets.Files) == 0 {
			return fmt.Errorf("no secrets.files declared in %s", secretsRulebook)
		}
		recCache := map[string][]age.Recipient{}
		ageStrCache := map[string][]string{}
		sealed, skipped, failed := 0, 0, 0
		var lastErr error
		for _, f := range rb.Secrets.Files {
			if !f.IsPair() {
				skipped++
				continue
			}
			e := toResolved(rb, f)
			srcAbs := f.DecryptedTo
			if !filepath.IsAbs(srcAbs) {
				srcAbs = filepath.Join(rb.Dir, srcAbs)
			}
			plain, err := os.ReadFile(srcAbs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAIL    %s: read plaintext %s: %v\n", f.Path, srcAbs, err)
				failed++
				lastErr = err
				continue
			}
			if secrets.LooksEncrypted(plain) {
				fmt.Fprintf(os.Stderr, "FAIL    %s: %s is already encrypted — refusing to double-encrypt\n", f.Path, srcAbs)
				failed++
				lastErr = fmt.Errorf("decrypted_to already encrypted")
				continue
			}
			useSops, sopsFormat, err := chooseFormat(e)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", f.Path, err)
				failed++
				lastErr = err
				continue
			}
			var (
				out  []byte
				mode string
			)
			if useSops {
				ageStrs, err := ageStringsForEntry(rb, e, ageStrCache)
				if err != nil {
					fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", f.Path, err)
					failed++
					lastErr = err
					continue
				}
				out, err = secrets.SopsEncryptBytes(plain, sopsFormat, ageStrs)
				if err != nil {
					fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", f.Path, err)
					failed++
					lastErr = err
					continue
				}
				mode = "sops/" + sopsFormat
			} else {
				recs, err := recsForEntry(rb, e, recCache)
				if err != nil {
					fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", f.Path, err)
					failed++
					lastErr = err
					continue
				}
				out, err = secrets.Encrypt(plain, recs)
				if err != nil {
					fmt.Fprintf(os.Stderr, "FAIL    %s: %v\n", f.Path, err)
					failed++
					lastErr = err
					continue
				}
				mode = "age"
			}
			if err := atomicWrite(e.absPath, out, 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "FAIL    %s: write: %v\n", f.Path, err)
				failed++
				lastErr = err
				continue
			}
			fmt.Printf("SEAL    %s → %s  (%s, recipients=%s)\n", f.DecryptedTo, f.Path, mode, e.recipientsLabel)
			sealed++
		}
		fmt.Printf("\nsealed=%d skipped=%d failed=%d\n", sealed, skipped, failed)
		if failed > 0 {
			return fmt.Errorf("%d pair file(s) failed to seal (last error: %w)", failed, lastErr)
		}
		return nil
	},
}

// secretsUnsealCmd walks pair entries and decrypts each `path` (encrypted)
// into `decrypted_to` (plaintext working copy). Auto-adds decrypted_to to
// the rulebook-dir .gitignore so the plaintext doesn't accidentally land
// in git.
var secretsUnsealCmd = &cobra.Command{
	Use:   "unseal",
	Short: "Decrypt every pair entry's `path` (encrypted) → `decrypted_to` (plaintext)",
	Long: `For every entry in secrets.files that has a decrypted_to: field, decrypt the
committed encrypted file and write the plaintext to decrypted_to. Mirrors
` + "`seal`" + `.

Run this before ` + "`axup deploy`" + ` if your deploy reads plaintext working
files (e.g. ` + "`--inventory inventory.prod.yaml`" + ` where the committed copy is
` + "`inventory.prod.enc.yaml`" + `). axup also auto-appends decrypted_to to the
nearest .gitignore so plaintext never accidentally lands in a commit.

Entries without decrypted_to: are skipped — for those, ` + "`secrets decrypt FILE`" + `
prints plaintext to stdout (no on-disk write).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		rb, err := loadRulebookHeader(secretsRulebook)
		if err != nil {
			return err
		}
		if rb.Secrets == nil || len(rb.Secrets.Files) == 0 {
			return fmt.Errorf("no secrets.files declared in %s", secretsRulebook)
		}
		unsealed, skipped, failed := 0, 0, 0
		var lastErr error
		var ignoreRels []string
		for _, f := range rb.Secrets.Files {
			if !f.IsPair() {
				skipped++
				continue
			}
			srcAbs := f.Path
			if !filepath.IsAbs(srcAbs) {
				srcAbs = filepath.Join(rb.Dir, srcAbs)
			}
			data, err := os.ReadFile(srcAbs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAIL    %s: read encrypted %s: %v\n", f.Path, srcAbs, err)
				failed++
				lastErr = err
				continue
			}
			if !secrets.LooksEncrypted(data) {
				fmt.Fprintf(os.Stderr, "FAIL    %s: %s is not encrypted — refusing to overwrite decrypted_to with raw bytes\n", f.Path, srcAbs)
				failed++
				lastErr = fmt.Errorf("path not encrypted")
				continue
			}
			plain, err := decryptForUnseal(srcAbs, data)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAIL    %s: decrypt: %v\n", f.Path, err)
				failed++
				lastErr = err
				continue
			}
			dstAbs := f.DecryptedTo
			if !filepath.IsAbs(dstAbs) {
				dstAbs = filepath.Join(rb.Dir, dstAbs)
			}
			if err := atomicWrite(dstAbs, plain, 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "FAIL    %s: write plaintext: %v\n", f.Path, err)
				failed++
				lastErr = err
				continue
			}
			fmt.Printf("UNSEAL  %s → %s  (mode=600)\n", f.Path, f.DecryptedTo)
			unsealed++
			ignoreRels = append(ignoreRels, f.DecryptedTo)
		}
		if len(ignoreRels) > 0 {
			added, err := ensureGitignored(rb.Dir, ignoreRels)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not update .gitignore: %v\n", err)
			} else if len(added) > 0 {
				fmt.Printf("\nappended %d entrie(s) to %s/.gitignore: %s\n",
					len(added), rb.Dir, strings.Join(added, ", "))
			}
		}
		fmt.Printf("\nunsealed=%d skipped=%d failed=%d\n", unsealed, skipped, failed)
		if failed > 0 {
			return fmt.Errorf("%d pair file(s) failed to unseal (last error: %w)", failed, lastErr)
		}
		return nil
	},
}

// decryptForUnseal mirrors the dispatch logic from `secrets decrypt FILE`:
// honour the explicit format hint from the file extension when applicable,
// otherwise fall through to the unified Decrypt() (which itself prefers the
// age armor header over the sops marker, see secrets.Decrypt).
func decryptForUnseal(path string, data []byte) ([]byte, error) {
	if secrets.LooksAgeEncrypted(data) {
		return secrets.Decrypt(data)
	}
	if format := secrets.SopsFormat(path); format != "" && secrets.LooksSopsEncrypted(data) {
		return secrets.SopsDecryptBytes(data, format)
	}
	return secrets.Decrypt(data)
}

// ensureGitignored makes sure each rel path is present in
// `<rulebookDir>/.gitignore`. Creates the file if missing. Returns the
// list of entries actually appended (existing ones are left alone).
//
// The match is exact-line, anchored — we treat `.gitignore` as a flat
// "set of paths" rather than parsing globs. That's good enough for the
// "decrypted_to landed in git" failure mode, and it never widens the
// ignore (we only add specific paths, never wildcards).
func ensureGitignored(dir string, rels []string) ([]string, error) {
	path := filepath.Join(dir, ".gitignore")
	var existing []string
	body, err := os.ReadFile(path)
	if err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			existing = append(existing, strings.TrimSpace(line))
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	have := map[string]bool{}
	for _, e := range existing {
		have[e] = true
	}
	var added []string
	for _, r := range rels {
		// Try both the bare relative path and a leading-slash anchored
		// form ("/foo") to be tolerant of both styles.
		if have[r] || have["/"+r] {
			continue
		}
		added = append(added, r)
	}
	if len(added) == 0 {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if len(body) > 0 && !strings.HasSuffix(string(body), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return nil, err
		}
	}
	if _, err := f.WriteString("\n# Plaintext working copies of axup secrets — never commit.\n# (axup secrets unseal manages this block.)\n"); err != nil {
		return nil, err
	}
	for _, r := range added {
		if _, err := f.WriteString(r + "\n"); err != nil {
			return nil, err
		}
	}
	return added, nil
}

var secretsStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the state (encrypted / plaintext / missing) of every file in secrets.files",
	RunE: func(cmd *cobra.Command, args []string) error {
		rb, err := loadRulebookHeader(secretsRulebook)
		if err != nil {
			return err
		}
		if rb.Secrets == nil || len(rb.Secrets.Files) == 0 {
			return fmt.Errorf("no secrets.files declared in %s", secretsRulebook)
		}
		defaultRec := secrets.RecipientsPath(rb.Dir, recipientsRel(rb))
		fmt.Printf("rulebook:    %s\n", secretsRulebook)
		if defaultRec == "" {
			fmt.Printf("recipients:  (none found)\n\n")
		} else {
			fmt.Printf("recipients:  %s\n\n", defaultRec)
		}
		bad := 0
		for _, f := range rb.Secrets.Files {
			path := f.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(rb.Dir, path)
			}
			label := "PLAINTEXT"
			data, err := os.ReadFile(path)
			switch {
			case os.IsNotExist(err):
				label = "MISSING"
				bad++
			case err != nil:
				return err
			case secrets.LooksSopsEncrypted(data):
				label = "encrypted (sops)"
			case secrets.LooksAgeEncrypted(data):
				label = "encrypted (age)"
			default:
				bad++
			}
			recLabel := ""
			if f.Recipients != "" {
				recLabel = "  (recipients=" + f.Recipients + ")"
			}
			fmt.Printf("  %-16s %s%s\n", label, f.Path, recLabel)
			// For pair entries, also report the state of decrypted_to: if
			// it exists on disk and isn't gitignored, that's a plaintext
			// leak risk. If it's missing, `unseal` will materialise it.
			if f.IsPair() {
				dst := f.DecryptedTo
				if !filepath.IsAbs(dst) {
					dst = filepath.Join(rb.Dir, dst)
				}
				dstLabel := "plaintext"
				if _, err := os.Stat(dst); os.IsNotExist(err) {
					dstLabel = "not-on-disk"
				} else if err != nil {
					return err
				}
				fmt.Printf("    └─ decrypted_to: %-12s %s\n", dstLabel, f.DecryptedTo)
			}
		}
		if bad > 0 {
			return fmt.Errorf("%d file(s) not in the expected encrypted state", bad)
		}
		return nil
	},
}

// recipientsRel returns the rulebook-relative recipients_file (or "" for default).
func recipientsRel(rb *rulebook.Rulebook) string {
	if rb.Secrets == nil {
		return ""
	}
	return rb.Secrets.RecipientsFile
}

// loadProjectRecipients resolves the rulebook-default recipients file path
// (respecting secrets.recipients_file if set) and parses it.
func loadProjectRecipients(rb *rulebook.Rulebook) ([]age.Recipient, error) {
	p := secrets.RecipientsPath(rb.Dir, recipientsRel(rb))
	return secrets.LoadRecipients(p)
}

// loadRecipientsAt parses an explicit per-file recipients override (rulebook-
// relative path is resolved against rb.Dir).
func loadRecipientsAt(rb *rulebook.Rulebook, rel string) ([]age.Recipient, error) {
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(rb.Dir, rel)
	}
	return secrets.LoadRecipients(p)
}

// resolvedEntry pairs a declared secrets-file entry with its absolute path and
// a human-friendly label for the recipients file actually used.
type resolvedEntry struct {
	entry           rulebook.SecretFile
	absPath         string
	recipientsLabel string // "recipients.txt" (default) or per-file override
}

// resolveSecretFiles assembles the list of entries to encrypt: either the
// single argv path, or every entry declared in secrets.files. Per-file
// recipients overrides ride along on each entry.
func resolveSecretFiles(rb *rulebook.Rulebook, args []string) ([]resolvedEntry, error) {
	if len(args) == 1 {
		ent := rulebook.SecretFile{Path: args[0]}
		// If this path matches a declared file, inherit its recipients override.
		if rb.Secrets != nil {
			for _, f := range rb.Secrets.Files {
				if samePath(rb.Dir, f.Path, args[0]) {
					ent = f
					break
				}
			}
		}
		return []resolvedEntry{toResolved(rb, ent)}, nil
	}
	if rb.Secrets == nil || len(rb.Secrets.Files) == 0 {
		return nil, fmt.Errorf("nothing to encrypt: pass a FILE arg or declare secrets.files in %s", secretsRulebook)
	}
	out := make([]resolvedEntry, 0, len(rb.Secrets.Files))
	for _, f := range rb.Secrets.Files {
		out = append(out, toResolved(rb, f))
	}
	return out, nil
}

func toResolved(rb *rulebook.Rulebook, f rulebook.SecretFile) resolvedEntry {
	abs := f.Path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(rb.Dir, abs)
	}
	label := recipientsRel(rb)
	if label == "" {
		label = "recipients.txt"
	}
	if f.Recipients != "" {
		label = f.Recipients
	}
	return resolvedEntry{entry: f, absPath: abs, recipientsLabel: label}
}

// recsForEntry loads (and caches) the recipients list for one entry. Falls
// back to the rulebook-default recipients when the entry has no override.
func recsForEntry(rb *rulebook.Rulebook, e resolvedEntry, cache map[string][]age.Recipient) ([]age.Recipient, error) {
	key := e.entry.Recipients
	if key == "" {
		key = "<default>"
	}
	if r, ok := cache[key]; ok {
		return r, nil
	}
	var (
		r   []age.Recipient
		err error
	)
	if e.entry.Recipients != "" {
		r, err = loadRecipientsAt(rb, e.entry.Recipients)
	} else {
		r, err = loadProjectRecipients(rb)
	}
	if err != nil {
		return nil, err
	}
	cache[key] = r
	return r, nil
}

// ageStringsForEntry is the sops-mode counterpart of recsForEntry —
// returns the raw `age1...` strings for sops's `--age` recipient list.
// Same per-file override + caching pattern.
func ageStringsForEntry(rb *rulebook.Rulebook, e resolvedEntry, cache map[string][]string) ([]string, error) {
	key := e.entry.Recipients
	if key == "" {
		key = "<default>"
	}
	if r, ok := cache[key]; ok {
		return r, nil
	}
	relOrDefault := e.entry.Recipients
	if relOrDefault == "" {
		relOrDefault = recipientsRel(rb)
	}
	p := secrets.RecipientsPath(rb.Dir, relOrDefault)
	strs, err := secrets.LoadAgeRecipientStrings(p)
	if err != nil {
		return nil, err
	}
	cache[key] = strs
	return strs, nil
}

// ageStringsForPath is the sops-mode counterpart of recipientsForPath,
// used by `axup secrets edit` when the file being edited is a sops
// format.
func ageStringsForPath(rb *rulebook.Rulebook, path string) ([]string, error) {
	rel := ""
	if rb.Secrets != nil {
		for _, f := range rb.Secrets.Files {
			if samePath(rb.Dir, f.Path, path) && f.Recipients != "" {
				rel = f.Recipients
				break
			}
		}
	}
	if rel == "" {
		rel = recipientsRel(rb)
	}
	p := secrets.RecipientsPath(rb.Dir, rel)
	return secrets.LoadAgeRecipientStrings(p)
}

// chooseFormat decides whether to encrypt one entry as sops-structural or as
// whole-file age. Honors an explicit `format:` in the secrets.files entry;
// otherwise falls back to extension-based auto-detection (sops for
// yaml/json/ini/env/toml, age for everything else).
//
// Returns (useSops, sopsFormat, err). When useSops is false, sopsFormat is "".
func chooseFormat(e resolvedEntry) (bool, string, error) {
	switch strings.ToLower(e.entry.Format) {
	case "age":
		return false, "", nil
	case "sops":
		f := secrets.SopsFormat(e.absPath)
		if f == "" {
			return false, "", fmt.Errorf("%s: format: sops requires a structured extension (.yaml/.yml/.json/.ini/.env/.toml)", e.entry.Path)
		}
		return true, f, nil
	case "", "auto":
		f := secrets.SopsFormat(e.absPath)
		return f != "", f, nil
	default:
		return false, "", fmt.Errorf("%s: unknown format %q (allowed: age, sops, auto)", e.entry.Path, e.entry.Format)
	}
}

// chooseFormatForPath resolves the dispatch for `axup secrets edit FILE`:
// look up FILE in secrets.files (honoring its `format:` override if any),
// or fall back to extension-based auto-detection when FILE isn't declared.
func chooseFormatForPath(rb *rulebook.Rulebook, path string) (bool, string, error) {
	if rb.Secrets != nil {
		for _, f := range rb.Secrets.Files {
			if samePath(rb.Dir, f.Path, path) {
				abs := f.Path
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(rb.Dir, abs)
				}
				return chooseFormat(resolvedEntry{entry: f, absPath: abs})
			}
		}
	}
	f := secrets.SopsFormat(path)
	return f != "", f, nil
}

// recipientsForPath picks recipients for `axup secrets edit FILE`: if FILE
// matches a declared secrets.files entry with its own recipients, use those;
// otherwise the rulebook default.
func recipientsForPath(rb *rulebook.Rulebook, path string) ([]age.Recipient, error) {
	if rb.Secrets != nil {
		for _, f := range rb.Secrets.Files {
			if samePath(rb.Dir, f.Path, path) && f.Recipients != "" {
				return loadRecipientsAt(rb, f.Recipients)
			}
		}
	}
	return loadProjectRecipients(rb)
}

// samePath compares a declared rulebook-relative path against the
// argv-provided path (which can be relative to CWD or absolute).
func samePath(rulebookDir, declared, given string) bool {
	a := declared
	if !filepath.IsAbs(a) {
		a = filepath.Join(rulebookDir, a)
	}
	b := given
	if !filepath.IsAbs(b) {
		b, _ = filepath.Abs(b)
	}
	aClean, _ := filepath.EvalSymlinks(a)
	bClean, _ := filepath.EvalSymlinks(b)
	if aClean == "" {
		aClean = filepath.Clean(a)
	}
	if bClean == "" {
		bClean = filepath.Clean(b)
	}
	return aClean == bClean
}

// rulebookDir returns the directory containing the rulebook so we can find
// recipients.txt alongside it. Falls back to the cwd when the file doesn't
// exist (e.g. running secrets commands from a fresh shell).
func rulebookDir() string {
	abs, err := filepath.Abs(secretsRulebook)
	if err == nil {
		return filepath.Dir(abs)
	}
	cwd, _ := os.Getwd()
	return cwd
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".deploy.tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func shellQuote(s string) string {
	return "'" + replaceAll(s, "'", `'\''`) + "'"
}
func replaceAll(s, old, new string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + new
		s = s[i+len(old):]
	}
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func init() {
	secretsCmd.PersistentFlags().StringVar(&secretsRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml (used to find recipients.txt + secrets.files)")
	secretsCmd.AddCommand(secretsEncryptCmd, secretsDecryptCmd, secretsEditCmd, secretsSealCmd, secretsUnsealCmd, secretsStatusCmd)
}
