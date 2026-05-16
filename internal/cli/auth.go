package cli

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// Auth flags are persistent so every subcommand that opens an SSH connection
// can use them. resolveAuth() turns the flag values into concrete strings
// (prompting on TTY when --ask-* flags are set).
var (
	sshKeyPath      string
	sshPassword     string
	sshAskPassword  bool
	sudoEnable      bool
	sudoPassword    string
	sudoAskPassword bool
)

type authResolved struct {
	KeyPath      string
	Password     string
	Sudo         bool
	SudoPassword string
}

func resolveAuth() (authResolved, error) {
	out := authResolved{
		KeyPath:      sshKeyPath,
		Password:     sshPassword,
		Sudo:         sudoEnable,
		SudoPassword: sudoPassword,
	}

	if sshAskPassword {
		if out.Password != "" {
			return authResolved{}, fmt.Errorf("--ask-password and --password are mutually exclusive")
		}
		pw, err := readPasswordTTY("SSH password: ", "--ask-password")
		if err != nil {
			return authResolved{}, err
		}
		out.Password = pw
	}

	// Providing a sudo password (interactive or direct) implies --sudo.
	if sudoAskPassword || out.SudoPassword != "" {
		out.Sudo = true
	}
	if sudoAskPassword {
		if out.SudoPassword != "" {
			return authResolved{}, fmt.Errorf("--ask-sudo-password and --sudo-password are mutually exclusive")
		}
		pw, err := readPasswordTTY("sudo password: ", "--ask-sudo-password")
		if err != nil {
			return authResolved{}, err
		}
		out.SudoPassword = pw
	}
	return out, nil
}

func readPasswordTTY(prompt, flagName string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", fmt.Errorf("%s requires an interactive TTY on stdin", flagName)
	}
	fmt.Fprint(os.Stderr, prompt)
	pw, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(pw), nil
}
