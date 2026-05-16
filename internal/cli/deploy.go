package cli

import (
	"github.com/spf13/cobra"

	"github.com/axgrid/axup/internal/runner"
)

var (
	deployHost      string
	deployGroup     string
	deployRulebook  string
	deployInventory string
	deployVarsFile  string
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Roll out a project to one or more configured servers using the rulebook's deploy tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := resolveAuth()
		if err != nil {
			return err
		}
		return runner.Run(runner.Options{
			Phase:         "deploy",
			Host:          deployHost,
			Group:         deployGroup,
			RulebookPath:  deployRulebook,
			InventoryPath: deployInventory,
			VarsFile:      deployVarsFile,
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
	deployCmd.Flags().StringVar(&deployHost, "host", "", "Target host (user@addr[:port]) or inventory host name")
	deployCmd.Flags().StringVar(&deployGroup, "group", "", "Inventory group name (mutex with --host)")
	deployCmd.Flags().StringVar(&deployRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
	deployCmd.Flags().StringVar(&deployInventory, "inventory", "", "Optional inventory YAML (overrides default inventory.yaml next to rulebook; may be age-encrypted)")
	deployCmd.Flags().StringVar(&deployVarsFile, "vars", "", "Optional YAML file with vars to merge into the rulebook (overrides rulebook defaults, inventory wins)")
}
