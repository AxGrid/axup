package transport

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

// AcceptNewHostKeys, when true, makes the host-key callback silently trust
// any host not yet present in ~/.ssh/known_hosts and append its key
// (StrictHostKeyChecking=accept-new in OpenSSH terms). When false (default)
// the callback prompts on a TTY before accepting; without a TTY it falls
// back to the standard `knownhosts.KeyError`. Mismatching keys (KeyError
// with non-empty Want) are always fatal — they require explicit user action.
var AcceptNewHostKeys bool

// hostKeyMu serializes prompts + appends so parallel multi-host SSH
// dialing doesn't interleave prompts on the terminal or corrupt
// known_hosts with concurrent writes.
var hostKeyMu sync.Mutex

type hostKeyConfig struct {
	callback   ssh.HostKeyCallback
	algorithms []string // algos found in known_hosts for the host; empty = use Go defaults
}

// resolveHostKey builds a HostKeyCallback for the given host. When
// ~/.ssh/known_hosts exists it is consulted, and we additionally extract the
// algorithms recorded for this host so the SSH handshake negotiates a key type
// the file knows about (otherwise Go can pick ECDSA even when only an ed25519
// line is recorded, producing a spurious "key mismatch"). Unknown hosts go
// through an OpenSSH-style yes/no/fingerprint prompt on a TTY.
func resolveHostKey(host Host) (hostKeyConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return hostKeyConfig{callback: ssh.InsecureIgnoreHostKey()}, nil
	}
	sshDir := filepath.Join(home, ".ssh")
	p := filepath.Join(sshDir, "known_hosts")
	if _, err := os.Stat(p); err != nil {
		if !os.IsNotExist(err) {
			return hostKeyConfig{}, fmt.Errorf("stat known_hosts: %w", err)
		}
		// First connection ever — create an empty known_hosts so the
		// prompt-and-append path below is uniform with the existing-file case.
		if mkErr := os.MkdirAll(sshDir, 0o700); mkErr != nil {
			return hostKeyConfig{callback: ssh.InsecureIgnoreHostKey()}, nil
		}
		f, openErr := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o600)
		if openErr != nil {
			return hostKeyConfig{callback: ssh.InsecureIgnoreHostKey()}, nil
		}
		_ = f.Close()
	}
	cb, err := knownhosts.New(p)
	if err != nil {
		return hostKeyConfig{}, fmt.Errorf("load known_hosts: %w", err)
	}
	return hostKeyConfig{
		callback:   wrapHostKeyCallback(cb, p),
		algorithms: scanHostAlgorithms(p, host.Addr, host.Port),
	}, nil
}

// wrapHostKeyCallback intercepts "host not in known_hosts" errors from the
// underlying knownhosts callback and resolves them by either auto-accepting
// (AcceptNewHostKeys) or prompting on a TTY. Mismatches (KeyError.Want non-empty)
// are passed through unchanged.
func wrapHostKeyCallback(inner ssh.HostKeyCallback, knownHostsPath string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := inner(hostname, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) || len(ke.Want) > 0 {
			return err
		}
		// Host is unknown — decide based on flag / TTY.
		hostKeyMu.Lock()
		defer hostKeyMu.Unlock()
		// Re-check under the lock: a sibling goroutine may have just added it.
		if err2 := inner(hostname, remote, key); err2 == nil {
			return nil
		}
		fp := ssh.FingerprintSHA256(key)
		if AcceptNewHostKeys {
			fmt.Fprintf(os.Stderr, "Warning: permanently adding %q (%s) to known hosts. Fingerprint: %s\n",
				hostname, key.Type(), fp)
			return appendKnownHost(knownHostsPath, hostname, remote, key)
		}
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("%w (re-run with --accept-new-hostkey to add it automatically, or run `ssh-keyscan -H %s >> ~/.ssh/known_hosts` first)",
				err, stripPort(hostname))
		}
		ok, perr := promptAcceptHostKey(hostname, key, fp)
		if perr != nil {
			return perr
		}
		if !ok {
			return fmt.Errorf("host key for %s rejected by user", hostname)
		}
		return appendKnownHost(knownHostsPath, hostname, remote, key)
	}
}

// promptAcceptHostKey mirrors OpenSSH's "yes/no/[fingerprint]" prompt so
// the question users see is the one they already know.
func promptAcceptHostKey(hostname string, key ssh.PublicKey, fp string) (bool, error) {
	fmt.Fprintf(os.Stderr, "The authenticity of host '%s' can't be established.\n", hostname)
	fmt.Fprintf(os.Stderr, "%s key fingerprint is %s.\n", strings.ToUpper(key.Type()), fp)
	fmt.Fprintln(os.Stderr, "This key is not known by any other names.")
	fmt.Fprint(os.Stderr, "Are you sure you want to continue connecting (yes/no/[fingerprint])? ")

	r := bufio.NewReader(os.Stdin)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return false, fmt.Errorf("read answer: %w", err)
		}
		ans := strings.TrimSpace(line)
		switch strings.ToLower(ans) {
		case "yes", "y":
			return true, nil
		case "no", "n", "":
			return false, nil
		}
		if ans == fp {
			return true, nil
		}
		fmt.Fprint(os.Stderr, "Please type 'yes', 'no' or the fingerprint: ")
	}
}

// appendKnownHost writes a single line to known_hosts covering both the
// hostname the user typed and the resolved remote address, so subsequent
// connects via either spelling match.
func appendKnownHost(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	hn := knownhosts.Normalize(hostname)
	addrs := []string{hn}
	if remote != nil {
		if a := knownhosts.Normalize(remote.String()); a != "" && a != hn {
			addrs = append(addrs, a)
		}
	}
	line := knownhosts.Line(addrs, key)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open known_hosts for append: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("append known_hosts: %w", err)
	}
	return nil
}

// stripPort returns "host" from "host:port" for the ssh-keyscan hint shown
// in non-interactive error messages. Leaves bracketed IPv6 alone.
func stripPort(hp string) string {
	if strings.HasPrefix(hp, "[") {
		if i := strings.Index(hp, "]"); i > 0 {
			return hp[1:i]
		}
		return hp
	}
	if i := strings.LastIndex(hp, ":"); i > 0 {
		return hp[:i]
	}
	return hp
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
