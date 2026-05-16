package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/axgrid/deploy/internal/runner"
)

var (
	runHost     string
	runGroup    string
	runRulebook string
	runVarsFile string
)

var runCmd = &cobra.Command{
	Use:   "run <phase>",
	Short: "Run any named phase from the rulebook (bootstrap / deploy / custom)",
	Long: `Run a single named phase from the rulebook against the chosen host(s).

A rulebook can declare any number of phases as top-level keys alongside the
built-in 'bootstrap:' and 'deploy:'. Example:

  bootstrap:    [...]
  deploy:       [...]
  deploy_crash: [...]
  migrate:      [...]

` + "`" + `deploy run deploy_crash` + "`" + ` runs the deploy_crash phase only.
Phase names must match ^[a-z][a-z0-9_-]*$ and cannot be 'status' or 'tasks'
(both are reserved by the CLI).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		phase := args[0]
		if phase == "" {
			return fmt.Errorf("phase argument is required")
		}
		a, err := resolveAuth()
		if err != nil {
			return err
		}
		return runner.Run(runner.Options{
			Phase:        phase,
			Host:         runHost,
			Group:        runGroup,
			RulebookPath: runRulebook,
			VarsFile:     runVarsFile,
			KeyPath:      a.KeyPath,
			Password:     a.Password,
			Sudo:         a.Sudo,
			SudoPassword: a.SudoPassword,
			DryRun:       dryRun,
		})
	},
}

func init() {
	runCmd.Flags().StringVar(&runHost, "host", "", "Target host (user@addr[:port]) or inventory host name")
	runCmd.Flags().StringVar(&runGroup, "group", "", "Inventory group name (mutex with --host)")
	runCmd.Flags().StringVar(&runRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
	runCmd.Flags().StringVar(&runVarsFile, "vars", "", "Optional YAML file with vars to merge into the rulebook (overrides rulebook defaults, inventory wins)")
}
