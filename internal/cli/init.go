package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Scaffold a rulebook.yaml in the current directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := os.Stat("rulebook.yaml"); err == nil {
			return fmt.Errorf("rulebook.yaml already exists")
		}
		if err := os.WriteFile("rulebook.yaml", []byte(rulebookTemplate), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote rulebook.yaml")
		return nil
	},
}

const rulebookTemplate = `# yaml-language-server: $schema=https://raw.githubusercontent.com/AxGrid/axup/main/schemas/rulebook.schema.json
name: my-app

vars:
  app_name: my-app

bootstrap:
  - name: ping
    command: "uname -a"

deploy:
  - name: hello
    command: "echo deploying my-app"
`
