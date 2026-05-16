package rulebook

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/axgrid/axup/internal/secrets"
)

// DockerCreds is the on-disk format for a docker_login creds_file. Plain YAML
// today; the same shape will hold once age/sops encryption is added (the CLI
// will detect ciphertext, decrypt, and parse the result).
type DockerCreds struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// LoadDockerCreds resolves a docker_login spec's credentials. When
// spec.CredsFile is set it is parsed from disk; otherwise the inline
// username/password sources on the spec are used.
func (rb *Rulebook) LoadDockerCreds(spec *DockerLoginSpec) (username, password string, err error) {
	if spec.CredsFile != "" {
		path := spec.CredsFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(rb.Dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("read creds_file %s: %w", spec.CredsFile, err)
		}
		// Transparently decrypt if the file is age-armored. Plaintext
		// passes through unchanged.
		data, err = secrets.Decrypt(data)
		if err != nil {
			return "", "", fmt.Errorf("decrypt creds_file %s: %w", spec.CredsFile, err)
		}
		var c DockerCreds
		if err := yaml.Unmarshal(data, &c); err != nil {
			return "", "", fmt.Errorf("parse creds_file %s: %w", spec.CredsFile, err)
		}
		if c.Username == "" || c.Password == "" {
			return "", "", fmt.Errorf("creds_file %s: both 'username' and 'password' are required", spec.CredsFile)
		}
		return c.Username, c.Password, nil
	}

	if spec.Username == "" {
		return "", "", fmt.Errorf("docker_login: username (or creds_file) is required")
	}
	pw, err := loadInlinePassword(rb.Dir, spec)
	if err != nil {
		return "", "", err
	}
	return spec.Username, pw, nil
}

func loadInlinePassword(dir string, spec *DockerLoginSpec) (string, error) {
	switch {
	case spec.Password != "":
		return spec.Password, nil
	case spec.PasswordFile != "":
		path := spec.PasswordFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read password_file: %w", err)
		}
		data, err = secrets.Decrypt(data)
		if err != nil {
			return "", fmt.Errorf("decrypt password_file %s: %w", spec.PasswordFile, err)
		}
		// Trim trailing newline editors add; most registries reject it.
		for len(data) > 0 && (data[len(data)-1] == '\n' || data[len(data)-1] == '\r') {
			data = data[:len(data)-1]
		}
		return string(data), nil
	case spec.PasswordEnv != "":
		v := os.Getenv(spec.PasswordEnv)
		if v == "" {
			return "", fmt.Errorf("env var %s is empty or unset", spec.PasswordEnv)
		}
		return v, nil
	}
	return "", fmt.Errorf("docker_login: no password source set (need creds_file or password/password_file/password_env)")
}
