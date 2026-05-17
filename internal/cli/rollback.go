package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/axgrid/axup/internal/runner"
)

var (
	rollbackHost      string
	rollbackGroup     string
	rollbackRulebook  string
	rollbackInventory string
	rollbackStep      int
	rollbackTask      string
	rollbackYes       bool
)

var rollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Restore tracked files from their on-host history chain",
	Long: `Reads ~/.axup-state/<rulebook>/state.json on each target host and restores
every tracked copy/template file from the archived history captured by previous
deploys.

Reset semantics: the rolled-over entries are discarded after restore — including
the version that was current before. "Go back to version N" means "version N+1
is obsolete and no longer reachable as a rollback target". A second rollback
goes one step further back, not forward.

Use --check / --dry-run to preview what would be restored without touching
anything. Use --history (under ` + "`axup status --history`" + `) to see the
chain before deciding which --step to pick.

Multi-host: each host has its own state.json + history, so rollback is fanned
out per-host. A host with no history is reported as an error but doesn't abort
the other hosts.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if rollbackStep < 1 {
			return fmt.Errorf("--step must be >= 1 (got %d)", rollbackStep)
		}
		// Confirmation prompt for non-trivial rollbacks. --check skips it
		// (it's a no-op anyway) and --yes bypasses it for scripts/CI.
		if rollbackStep > 1 && !rollbackYes && !dryRun {
			fmt.Fprintf(os.Stderr, "About to roll back %d steps on host=%q group=%q (reset semantics: %d versions will be dropped).\nProceed? [y/N] ",
				rollbackStep, rollbackHost, rollbackGroup, rollbackStep)
			reader := bufio.NewReader(os.Stdin)
			line, _ := reader.ReadString('\n')
			ans := strings.ToLower(strings.TrimSpace(line))
			if ans != "y" && ans != "yes" {
				fmt.Fprintln(os.Stderr, "aborted")
				return nil
			}
		}
		a, err := resolveAuth()
		if err != nil {
			return err
		}
		return runner.Run(runner.Options{
			Host:          rollbackHost,
			Group:         rollbackGroup,
			RulebookPath:  rollbackRulebook,
			InventoryPath: rollbackInventory,
			KeyPath:       a.KeyPath,
			Password:      a.Password,
			Sudo:          a.Sudo,
			SudoPassword:  a.SudoPassword,
			DryRun:        dryRun,
			Rollback:      true,
			RollbackStep:  rollbackStep,
			RollbackTask:  rollbackTask,
		})
	},
}

func init() {
	rollbackCmd.Flags().StringVar(&rollbackHost, "host", "", "Target host (user@addr[:port]) or inventory host name")
	rollbackCmd.Flags().StringVar(&rollbackGroup, "group", "", "Inventory group name (mutex with --host)")
	rollbackCmd.Flags().StringVar(&rollbackRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
	rollbackCmd.Flags().StringVar(&rollbackInventory, "inventory", "", "Optional inventory YAML (overrides default inventory.yaml next to rulebook; may be age-encrypted)")
	rollbackCmd.Flags().IntVar(&rollbackStep, "step", 1, "How many versions back to restore (1 = previous)")
	rollbackCmd.Flags().StringVar(&rollbackTask, "task", "", "Optional: only roll back this single tracked path (full dst path, e.g. /opt/foo/bin)")
	rollbackCmd.Flags().BoolVar(&rollbackYes, "yes", false, "Skip the confirmation prompt for --step > 1 (use in scripts/CI)")
}
