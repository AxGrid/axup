package cli

import (
	"github.com/spf13/cobra"

	"github.com/axgrid/deploy/internal/runner"
)

var (
	deployHost     string
	deployRulebook string
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Roll out a project to a configured server using the rulebook's deploy tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := resolveAuth()
		if err != nil {
			return err
		}
		return runner.Run(runner.Options{
			Phase:        "deploy",
			Host:         deployHost,
			RulebookPath: deployRulebook,
			KeyPath:      a.KeyPath,
			Password:     a.Password,
			Sudo:         a.Sudo,
			SudoPassword: a.SudoPassword,
		})
	},
}

func init() {
	deployCmd.Flags().StringVar(&deployHost, "host", "", "Target host (user@addr[:port])")
	deployCmd.Flags().StringVar(&deployRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
	_ = deployCmd.MarkFlagRequired("host")
}
