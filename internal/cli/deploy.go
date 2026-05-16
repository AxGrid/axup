package cli

import (
	"github.com/spf13/cobra"

	"github.com/axgrid/deploy/internal/runner"
)

var (
	deployHost     string
	deployGroup    string
	deployRulebook string
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
			Phase:        "deploy",
			Host:         deployHost,
			Group:        deployGroup,
			RulebookPath: deployRulebook,
			KeyPath:      a.KeyPath,
			Password:     a.Password,
			Sudo:         a.Sudo,
			SudoPassword: a.SudoPassword,
			DryRun:       dryRun,
		})
	},
}

func init() {
	deployCmd.Flags().StringVar(&deployHost, "host", "", "Target host (user@addr[:port]) or inventory host name")
	deployCmd.Flags().StringVar(&deployGroup, "group", "", "Inventory group name (mutex with --host)")
	deployCmd.Flags().StringVar(&deployRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
}
