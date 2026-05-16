package cli

import (
	"github.com/spf13/cobra"

	"github.com/axgrid/axup/internal/runner"
)

var (
	bootstrapHost      string
	bootstrapGroup     string
	bootstrapRulebook  string
	bootstrapInventory string
	bootstrapVarsFile  string
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Configure one or more servers using the rulebook's bootstrap tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := resolveAuth()
		if err != nil {
			return err
		}
		return runner.Run(runner.Options{
			Phase:         "bootstrap",
			Host:          bootstrapHost,
			Group:         bootstrapGroup,
			RulebookPath:  bootstrapRulebook,
			InventoryPath: bootstrapInventory,
			VarsFile:      bootstrapVarsFile,
			KeyPath:       a.KeyPath,
			Password:      a.Password,
			Sudo:          a.Sudo,
			SudoPassword:  a.SudoPassword,
			DryRun:        dryRun || dryRunDiff,
			Diff:          dryRunDiff,
		})
	},
}

func init() {
	bootstrapCmd.Flags().StringVar(&bootstrapHost, "host", "", "Target host (user@addr[:port]) or inventory host name")
	bootstrapCmd.Flags().StringVar(&bootstrapGroup, "group", "", "Inventory group name (mutex with --host)")
	bootstrapCmd.Flags().StringVar(&bootstrapRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
	bootstrapCmd.Flags().StringVar(&bootstrapInventory, "inventory", "", "Optional inventory YAML (overrides default inventory.yaml next to rulebook; may be age-encrypted)")
	bootstrapCmd.Flags().StringVar(&bootstrapVarsFile, "vars", "", "Optional YAML file with vars to merge into the rulebook (overrides rulebook defaults, inventory wins)")
}
