package cli

import (
	"github.com/spf13/cobra"

	"github.com/axgrid/deploy/internal/runner"
	"github.com/axgrid/deploy/internal/secrets"
)

var Version = "dev"

var (
	dryRun      bool
	noColor     bool
	ageKeyPaths []string
)

var rootCmd = &cobra.Command{
	Use:           "deploy",
	Short:         "Lightweight ansible-style deploy utility",
	Long:          "deploy bootstraps servers and rolls out projects via rulebook.yaml playbooks over SSH.",
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Sync persistent flags into packages that read them at run time.
		runner.NoColor = noColor
		secrets.KeyPaths = ageKeyPaths
	},
}

func Execute() error { return rootCmd.Execute() }

func init() {
	rootCmd.PersistentFlags().StringVar(&sshKeyPath, "key", "", "Path to SSH private key (overrides ssh-agent / default key discovery)")
	rootCmd.PersistentFlags().StringVar(&sshPassword, "password", "", "SSH password (insecure on shared machines; prefer --ask-password)")
	rootCmd.PersistentFlags().BoolVar(&sshAskPassword, "ask-password", false, "Prompt for SSH password on TTY")
	rootCmd.PersistentFlags().BoolVar(&sudoEnable, "sudo", false, "Run the agent under sudo -H -S (assumes NOPASSWD unless --sudo-password / --ask-sudo-password is given)")
	rootCmd.PersistentFlags().StringVar(&sudoPassword, "sudo-password", "", "Sudo password (insecure; prefer --ask-sudo-password); implies --sudo")
	rootCmd.PersistentFlags().BoolVar(&sudoAskPassword, "ask-sudo-password", false, "Prompt for sudo password on TTY; implies --sudo")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "check", false, "Preview tasks without applying any changes (alias: --dry-run)")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Alias for --check")
	rootCmd.PersistentFlags().BoolVar(&noColor, "no-color", false, "Disable ANSI colors (auto-disabled when stdout is not a TTY)")
	rootCmd.PersistentFlags().StringSliceVar(&ageKeyPaths, "age-key", nil, "Path to age identity file (repeatable; overrides auto-discovery; $DEPLOY_AGE_KEY is a path-list alternative)")
	rootCmd.AddCommand(versionCmd, initCmd, bootstrapCmd, deployCmd, depsCmd, secretsCmd, statusCmd)
}
