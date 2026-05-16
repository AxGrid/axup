package cli

import (
	"github.com/spf13/cobra"

	"github.com/axgrid/axup/internal/runner"
)

var (
	statusHost     string
	statusGroup    string
	statusRulebook string
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report each host's recorded state and any drift from it",
	Long: `Connects to one or more configured hosts, reads /root/.axup-state/<rulebook>/state.json,
and reports each tracked file's status (in_sync / drift / missing).

The agent runs in read-only mode — no changes are applied and the state file
is not rewritten.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := resolveAuth()
		if err != nil {
			return err
		}
		return runner.Run(runner.Options{
			Host:         statusHost,
			Group:        statusGroup,
			RulebookPath: statusRulebook,
			KeyPath:      a.KeyPath,
			Password:     a.Password,
			Sudo:         a.Sudo,
			SudoPassword: a.SudoPassword,
			StatusOnly:   true,
		})
	},
}

func init() {
	statusCmd.Flags().StringVar(&statusHost, "host", "", "Target host (user@addr[:port]) or inventory host name")
	statusCmd.Flags().StringVar(&statusGroup, "group", "", "Inventory group name (mutex with --host)")
	statusCmd.Flags().StringVar(&statusRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
}
