package transport

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// AuthOptions controls which credentials Dial will try. If KeyPath or Password
// is set, *only* the explicit options are used (we don't fall back to
// ssh-agent / default keys, so behavior matches what the user asked for). If
// both are empty, we auto-discover: ssh-agent first, then ~/.ssh/id_*.
type AuthOptions struct {
	KeyPath  string // explicit private key file
	Password string // explicit password for password auth
}

// buildAuthMethods turns the options into ssh.AuthMethods in the order the SSH
// client should try them.
func buildAuthMethods(opts AuthOptions) ([]ssh.AuthMethod, error) {
	if opts.KeyPath != "" || opts.Password != "" {
		var methods []ssh.AuthMethod
		if opts.KeyPath != "" {
			data, err := os.ReadFile(opts.KeyPath)
			if err != nil {
				return nil, fmt.Errorf("read key %s: %w", opts.KeyPath, err)
			}
			signer, err := ssh.ParsePrivateKey(data)
			if err != nil {
				return nil, fmt.Errorf("parse key %s: %w (encrypted keys are not supported yet — use ssh-add and omit --key)", opts.KeyPath, err)
			}
			methods = append(methods, ssh.PublicKeys(signer))
		}
		if opts.Password != "" {
			methods = append(methods, ssh.Password(opts.Password))
		}
		return methods, nil
	}

	return discoverAuthMethods()
}

func discoverAuthMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return methods, nil
	}
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		p := filepath.Join(home, ".ssh", name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no SSH auth methods available (no SSH_AUTH_SOCK, no ~/.ssh/id_* keys, and no --key / --password / --ask-password given)")
	}
	return methods, nil
}
