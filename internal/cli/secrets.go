package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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
		recs, err := loadProjectRecipients(rb)
		if err != nil {
			return err
		}
		var files []string
		switch {
		case len(args) == 1:
			files = []string{args[0]}
		case rb.Secrets != nil && len(rb.Secrets.Files) > 0:
			for _, f := range rb.Secrets.Files {
				if !filepath.IsAbs(f) {
					f = filepath.Join(rb.Dir, f)
				}
				files = append(files, f)
			}
		default:
			return fmt.Errorf("nothing to encrypt: pass a FILE arg or declare secrets.files in %s", secretsRulebook)
		}

		encrypted, skipped, missing := 0, 0, 0
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Printf("MISSING %s (declared but not on disk)\n", path)
					missing++
					continue
				}
				return fmt.Errorf("read %s: %w", path, err)
			}
			if secrets.LooksEncrypted(data) {
				fmt.Printf("skip    %s (already encrypted)\n", path)
				skipped++
				continue
			}
			out, err := secrets.Encrypt(data, recs)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if err := atomicWrite(path, out, 0o600); err != nil {
				return err
			}
			fmt.Printf("OK      %s\n", path)
			encrypted++
		}
		fmt.Printf("\nencrypted=%d skipped=%d missing=%d (recipients=%d)\n", encrypted, skipped, missing, len(recs))
		if missing > 0 {
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
		out, err := secrets.Decrypt(data)
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
		recs, err := loadProjectRecipients(rb)
		if err != nil {
			return err
		}
		// Read + decrypt (or accept plain if file doesn't exist yet).
		var plain []byte
		if data, err := os.ReadFile(path); err == nil {
			plain, err = secrets.Decrypt(data)
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
		out, err := secrets.Encrypt(edited, recs)
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
		recPath := secrets.RecipientsPath(rb.Dir, recipientsRel(rb))
		fmt.Printf("rulebook:    %s\n", secretsRulebook)
		if recPath == "" {
			fmt.Printf("recipients:  (none found)\n\n")
		} else {
			fmt.Printf("recipients:  %s\n\n", recPath)
		}
		bad := 0
		for _, f := range rb.Secrets.Files {
			path := f
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
			case secrets.LooksEncrypted(data):
				label = "encrypted"
			default:
				bad++
			}
			fmt.Printf("  %-10s %s\n", label, f)
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

// loadProjectRecipients resolves the recipients file path against the rulebook
// (respecting secrets.recipients_file if set) and parses it.
func loadProjectRecipients(rb *rulebook.Rulebook) ([]age.Recipient, error) {
	p := secrets.RecipientsPath(rb.Dir, recipientsRel(rb))
	return secrets.LoadRecipients(p)
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
	secretsCmd.AddCommand(secretsEncryptCmd, secretsDecryptCmd, secretsEditCmd, secretsStatusCmd)
}
