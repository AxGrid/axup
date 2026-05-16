// Package inventory parses inventory.yaml: a list of named hosts with their
// address / user / port / per-host vars, plus named groups that bundle hosts
// together. The CLI's --host and --group flags resolve against it.
package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/axgrid/axup/internal/secrets"
)

const DefaultFileName = "inventory.yaml"

type Inventory struct {
	Hosts  map[string]*Host    `yaml:"hosts"`
	Groups map[string][]string `yaml:"groups"`

	// Path is the file the inventory was loaded from. Empty when no inventory
	// is present — the CLI's --host can still accept "user@addr" literals.
	Path string `yaml:"-"`
}

type Host struct {
	Address string         `yaml:"address"`
	User    string         `yaml:"user,omitempty"`
	Port    string         `yaml:"port,omitempty"`
	Vars    map[string]any `yaml:"vars,omitempty"`
}

// Resolved is the projection used by the runner: a logical name plus the
// concrete SSH spec and per-host vars to merge into rb.Vars.
type Resolved struct {
	Name    string
	Spec    string // "user@addr[:port]" suitable for transport.Dial
	Vars    map[string]any
	AdHoc   bool // true when this came from a bare --host user@addr (not inventory)
}

// LoadDir tries to load inventory.yaml from `dir`. Returns (nil, nil) when the
// file is absent — inventory is optional. For an explicit per-env file pass
// the path to LoadPath instead.
func LoadDir(dir string) (*Inventory, error) {
	p := filepath.Join(dir, DefaultFileName)
	inv, err := LoadPath(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return inv, err
}

// LoadPath reads an inventory YAML from an explicit path. The file may be
// age-encrypted (text-armor); decryption uses the same identity discovery as
// `docker_login.creds_file` (--age-key flag, $AXUP_AGE_KEY, auto-discovered
// keys). Returns os.ErrNotExist (wrapped) if the file is absent, so callers
// that want the optional-default behaviour can branch with errors.Is.
func LoadPath(path string) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if secrets.LooksEncrypted(data) {
		plain, err := secrets.Decrypt(data)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", path, err)
		}
		data = plain
	}
	var inv Inventory
	if err := yaml.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	inv.Path = path
	if err := inv.validate(); err != nil {
		return nil, err
	}
	return &inv, nil
}

func (inv *Inventory) validate() error {
	for name, h := range inv.Hosts {
		if h == nil || h.Address == "" {
			return fmt.Errorf("inventory host %q: address is required", name)
		}
	}
	for g, members := range inv.Groups {
		for _, m := range members {
			if _, ok := inv.Hosts[m]; !ok {
				return fmt.Errorf("inventory group %q: member %q is not a declared host", g, m)
			}
		}
	}
	return nil
}

// Resolve turns a --host or --group identifier into a list of Resolved hosts.
// Lookup order:
//
//   - if `host` contains '@' or is an IP/FQDN that doesn't appear as a host
//     name, it's treated as an ad-hoc literal "user@addr[:port]"
//   - else if `host` matches an inventory.Hosts key, that single host is returned
//   - else if `group` is non-empty and matches an inventory.Groups key, every
//     host in the group is returned, in declaration order
func (inv *Inventory) Resolve(host, group string) ([]Resolved, error) {
	if host == "" && group == "" {
		return nil, fmt.Errorf("either --host or --group is required")
	}
	if host != "" && group != "" {
		return nil, fmt.Errorf("--host and --group are mutually exclusive")
	}

	if host != "" {
		if inv != nil {
			if h, ok := inv.Hosts[host]; ok {
				return []Resolved{makeResolved(host, h)}, nil
			}
		}
		// Treat as ad-hoc "user@addr" form.
		if !looksLikeAdHocHost(host) {
			if inv == nil {
				return nil, fmt.Errorf("host %q has no @ separator and no inventory.yaml was found", host)
			}
			return nil, fmt.Errorf("host %q not found in inventory.yaml (declared hosts: %s)", host, joinKeys(inv.Hosts))
		}
		return []Resolved{{Name: host, Spec: host, AdHoc: true}}, nil
	}

	// group != ""
	if inv == nil {
		return nil, fmt.Errorf("--group %q used but no inventory.yaml was found in the rulebook dir", group)
	}
	members, ok := inv.Groups[group]
	if !ok {
		return nil, fmt.Errorf("group %q not found in inventory.yaml (declared groups: %s)", group, joinKeys(inv.Groups))
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("group %q is empty", group)
	}
	out := make([]Resolved, 0, len(members))
	for _, m := range members {
		out = append(out, makeResolved(m, inv.Hosts[m]))
	}
	return out, nil
}

func makeResolved(name string, h *Host) Resolved {
	user := h.User
	if user == "" {
		user = "root"
	}
	addr := h.Address
	port := h.Port
	spec := user + "@" + addr
	if port != "" && port != "22" {
		spec += ":" + port
	}
	return Resolved{Name: name, Spec: spec, Vars: h.Vars}
}

// looksLikeAdHocHost returns true if the string contains '@' (the SSH "user@host"
// separator). Without the '@' we don't try to be clever — names without it
// must match an inventory entry.
func looksLikeAdHocHost(s string) bool {
	return strings.Contains(s, "@")
}

func joinKeys[V any](m map[string]V) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}
