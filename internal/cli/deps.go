package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/axgrid/deploy/internal/rulebook"
)

var (
	depsRulebook string
)

var depsCmd = &cobra.Command{
	Use:   "deps",
	Short: "Manage external rulebook dependencies",
}

var depsTidyCmd = &cobra.Command{
	Use:   "tidy",
	Short: "Resolve all deps to fresh SHAs and rewrite deploy.lock",
	RunE: func(cmd *cobra.Command, args []string) error {
		rb, err := loadRulebookHeader(depsRulebook)
		if err != nil {
			return err
		}
		if len(rb.Deps) == 0 {
			fmt.Println("no deps declared; nothing to tidy")
			return nil
		}
		lock, err := rulebook.RefreshAll(rb.Deps)
		if err != nil {
			return err
		}
		if err := rulebook.SaveLock(rb.Dir, lock); err != nil {
			return err
		}
		fmt.Printf("wrote %s with %d dep(s)\n", filepath.Join(rb.Dir, rulebook.LockFileName), len(lock.Deps))
		for _, d := range lock.Deps {
			fmt.Printf("  %s %s %s -> %s\n", d.Name, d.Git, d.Version, d.Sha[:12])
		}
		return nil
	},
}

var depsVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify deploy.lock matches the declared deps (no network resolution)",
	RunE: func(cmd *cobra.Command, args []string) error {
		rb, err := loadRulebookHeader(depsRulebook)
		if err != nil {
			return err
		}
		lock, err := rulebook.LoadLock(rb.Dir)
		if err != nil {
			return err
		}
		if lock == nil {
			return fmt.Errorf("no %s in %s — run `deploy deps tidy`", rulebook.LockFileName, rb.Dir)
		}
		missing := 0
		for _, d := range rb.Deps {
			entry := lock.Find(d.Name)
			if entry == nil {
				fmt.Printf("MISSING %s (declared but not locked)\n", d.Name)
				missing++
				continue
			}
			if entry.Git != d.Git || entry.Version != d.Version {
				fmt.Printf("DRIFT  %s — rulebook=(git=%s, version=%s), lock=(git=%s, version=%s)\n",
					d.Name, d.Git, d.Version, entry.Git, entry.Version)
				missing++
				continue
			}
			fmt.Printf("OK     %s %s %s @ %s\n", d.Name, d.Git, d.Version, entry.Sha[:12])
		}
		if missing > 0 {
			return fmt.Errorf("%d dep(s) need `deploy deps tidy`", missing)
		}
		return nil
	},
}

// loadRulebookHeader parses only enough of the rulebook to read Name + Deps +
// Dir. We do NOT call rulebook.Load here because that would force dep
// resolution, which is exactly what we're trying to manage.
func loadRulebookHeader(path string) (*rulebook.Rulebook, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rulebook %s: %w", path, err)
	}
	var rb rulebook.Rulebook
	if err := yaml.Unmarshal(data, &rb); err != nil {
		return nil, fmt.Errorf("parse rulebook %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	rb.Dir = filepath.Dir(abs)
	return &rb, nil
}

func init() {
	depsCmd.PersistentFlags().StringVar(&depsRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
	depsCmd.AddCommand(depsTidyCmd, depsVerifyCmd)
}
