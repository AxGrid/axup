package cli

import "github.com/spf13/cobra"

var Version = "dev"

var rootCmd = &cobra.Command{
	Use:           "deploy",
	Short:         "Lightweight ansible-style deploy utility",
	Long:          "deploy bootstraps servers and rolls out projects via rulebook.yaml playbooks over SSH.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() error { return rootCmd.Execute() }

func init() {
	rootCmd.PersistentFlags().StringVar(&sshKeyPath, "key", "", "Path to SSH private key (overrides ssh-agent / default key discovery)")
	rootCmd.PersistentFlags().StringVar(&sshPassword, "password", "", "SSH password (insecure on shared machines; prefer --ask-password)")
	rootCmd.PersistentFlags().BoolVar(&sshAskPassword, "ask-password", false, "Prompt for SSH password on TTY")
	rootCmd.PersistentFlags().BoolVar(&sudoEnable, "sudo", false, "Run the agent under sudo -H -S (assumes NOPASSWD unless --sudo-password / --ask-sudo-password is given)")
	rootCmd.PersistentFlags().StringVar(&sudoPassword, "sudo-password", "", "Sudo password (insecure; prefer --ask-sudo-password); implies --sudo")
	rootCmd.PersistentFlags().BoolVar(&sudoAskPassword, "ask-sudo-password", false, "Prompt for sudo password on TTY; implies --sudo")
	rootCmd.AddCommand(versionCmd, initCmd, bootstrapCmd, deployCmd)
}
