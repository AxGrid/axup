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
	historyHost      string
	historyGroup     string
	historyRulebook  string
	historyInventory string
	historyYes       bool
)

var historyCmd = &cobra.Command{
	Use:   "history",
	Short: "Manage the per-host rollback-history chain",
	Long: `Subcommands operate on the history captured by deploys when the rulebook
sets ` + "`history: N`" + `. Currently only ` + "`clear`" + ` is implemented; ` + "`axup status --history`" + `
shows the chain without modification, ` + "`axup rollback`" + ` consumes it.`,
}

var historyClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Wipe every tracked file's history chain on the target host(s)",
	Long: `Removes every FileState.History entry and deletes the per-host history dir
(~/.axup-state/<rulebook>/history/). This is irreversible — after clear, any
` + "`axup rollback`" + ` will skip every file with "no history".

Typical use: after a deploy that includes an irreversible step (DB migration,
data format change) where rolling back the previous binary would fail at
runtime, run ` + "`axup history clear`" + ` to mark earlier versions as
unreachable rollback targets:

    axup deploy   --host ... --rulebook ...
    axup history clear --host ... --rulebook ...

` + "`--check`" + ` previews the count without touching anything.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Confirmation prompt — this is destructive and silent on disk
		// (no diff to review). --yes for scripts/CI, --check to preview.
		if !historyYes && !dryRun {
			fmt.Fprintf(os.Stderr, "About to wipe history on host=%q group=%q. This is irreversible.\nProceed? [y/N] ",
				historyHost, historyGroup)
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
			Host:          historyHost,
			Group:         historyGroup,
			RulebookPath:  historyRulebook,
			InventoryPath: historyInventory,
			KeyPath:       a.KeyPath,
			Password:      a.Password,
			Sudo:          a.Sudo,
			SudoPassword:  a.SudoPassword,
			DryRun:        dryRun,
			ClearHistory:  true,
		})
	},
}

func init() {
	historyClearCmd.Flags().StringVar(&historyHost, "host", "", "Target host (user@addr[:port]) or inventory host name")
	historyClearCmd.Flags().StringVar(&historyGroup, "group", "", "Inventory group name (mutex with --host)")
	historyClearCmd.Flags().StringVar(&historyRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
	historyClearCmd.Flags().StringVar(&historyInventory, "inventory", "", "Optional inventory YAML (overrides default inventory.yaml next to rulebook; may be age-encrypted)")
	historyClearCmd.Flags().BoolVar(&historyYes, "yes", false, "Skip the confirmation prompt (use in scripts/CI)")
	historyCmd.AddCommand(historyClearCmd)
}
