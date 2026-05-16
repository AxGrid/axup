package transport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type hostKeyConfig struct {
	callback   ssh.HostKeyCallback
	algorithms []string // algos found in known_hosts for the host; empty = use Go defaults
}

// resolveHostKey builds a HostKeyCallback for the given host. When
// ~/.ssh/known_hosts exists it is consulted, and we additionally extract the
// algorithms recorded for this host so the SSH handshake negotiates a key type
// the file knows about (otherwise Go can pick ECDSA even when only an ed25519
// line is recorded, producing a spurious "key mismatch").
func resolveHostKey(host Host) (hostKeyConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return hostKeyConfig{callback: ssh.InsecureIgnoreHostKey()}, nil
	}
	p := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(p); err != nil {
		return hostKeyConfig{callback: ssh.InsecureIgnoreHostKey()}, nil
	}
	cb, err := knownhosts.New(p)
	if err != nil {
		return hostKeyConfig{}, fmt.Errorf("load known_hosts: %w", err)
	}
	return hostKeyConfig{
		callback:   cb,
		algorithms: scanHostAlgorithms(p, host.Addr, host.Port),
	}, nil
}

// scanHostAlgorithms reads known_hosts and returns the host-key algorithms
// declared for the given address (preserving file order). Hashed entries
// (lines starting with "|1|") are skipped because matching them requires
// recomputing the salted HMAC for every candidate hostname — for our purposes
// the unhashed entries the user actually wrote are enough.
func scanHostAlgorithms(path, addr, port string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var bracketed string
	if port != "" && port != "22" {
		bracketed = "[" + addr + "]:" + port
	}
	var algos []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		hostsField := fields[0]
		if strings.HasPrefix(hostsField, "|") {
			continue
		}
		for _, h := range strings.Split(hostsField, ",") {
			if h == addr || (bracketed != "" && h == bracketed) {
				if !seen[fields[1]] {
					seen[fields[1]] = true
					algos = append(algos, fields[1])
				}
				break
			}
		}
	}
	return algos
}
