package rulebook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// CacheDir returns the on-disk location where cloned dep repos live.
// Layout: <cacheRoot>/git/<host-stripped-of-port>/<sanitized-path>@<sha>/
func CacheDir() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "axup"), nil
}

// NormalizedURL turns "github.com/foo/bar" into "https://github.com/foo/bar.git",
// leaves "git@…" and "https://…" alone, and accepts "file://" for tests.
func (d DepSpec) NormalizedURL() string {
	g := strings.TrimSpace(d.Git)
	switch {
	case strings.HasPrefix(g, "https://"), strings.HasPrefix(g, "http://"),
		strings.HasPrefix(g, "ssh://"), strings.HasPrefix(g, "file://"),
		strings.HasPrefix(g, "git@"):
		return g
	}
	if !strings.HasSuffix(g, ".git") {
		g += ".git"
	}
	return "https://" + g
}

// pathKey produces a filesystem-safe directory name unique to (git, sha). It's
// derived from the URL so identical repos cloned via different transports
// (https vs ssh) end up in the same cache row, plus the sha so different
// refs of the same repo coexist.
func pathKey(gitURL, sha string) string {
	// Strip user-info, normalize to host/path
	u, err := url.Parse(gitURL)
	host, path := "unknown", gitURL
	if err == nil && u.Host != "" {
		host = u.Host
		path = u.Path
	} else if strings.HasPrefix(gitURL, "git@") {
		// git@host:path[.git]
		rest := strings.TrimPrefix(gitURL, "git@")
		if i := strings.Index(rest, ":"); i >= 0 {
			host = rest[:i]
			path = rest[i+1:]
		}
	}
	path = strings.TrimSuffix(strings.TrimPrefix(path, "/"), ".git")
	path = sanitize(path)
	// Add a content hash suffix so collisions on long paths can't happen.
	sum := sha256.Sum256([]byte(gitURL))
	return fmt.Sprintf("%s/%s@%s.%s", host, path, sha, hex.EncodeToString(sum[:4]))
}

var unsafeRune = regexp.MustCompile(`[^A-Za-z0-9._/-]`)

func sanitize(s string) string {
	return unsafeRune.ReplaceAllString(s, "_")
}

// ResolveRef runs `git ls-remote` to resolve a ref (tag, branch) to its sha.
// If `ref` already looks like a 40-char hex sha we use it directly without
// hitting the network.
func ResolveRef(gitURL, ref string) (string, error) {
	if isSha(ref) {
		return strings.ToLower(ref), nil
	}
	// Prefer annotated tag dereference (^{}), fall back to plain ref. Asking
	// for both lets a single ls-remote serve tags and branches.
	cmd := exec.Command("git", "ls-remote", "--exit-code", gitURL,
		ref, "refs/tags/"+ref+"^{}", "refs/tags/"+ref, "refs/heads/"+ref)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git ls-remote %s %s: %w (stderr: %s)", gitURL, ref, err, strings.TrimSpace(stderr.String()))
	}
	// Output: <sha>\t<refname>\n... Prefer the dereferenced tag (^{}) if present.
	var deref, first string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if first == "" {
			first = fields[0]
		}
		if strings.HasSuffix(fields[1], "^{}") {
			deref = fields[0]
		}
	}
	if deref != "" {
		return strings.ToLower(deref), nil
	}
	if first == "" {
		return "", fmt.Errorf("git ls-remote: ref %q not found in %s", ref, gitURL)
	}
	return strings.ToLower(first), nil
}

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

func isSha(s string) bool { return shaRe.MatchString(s) }

// EnsureClone makes sure <cacheDir>/git/<key> contains the repo at exactly
// `sha`. If the directory already exists with the expected sha it's a no-op.
func EnsureClone(gitURL, sha, cacheDir string) (string, error) {
	dest := filepath.Join(cacheDir, "git", pathKey(gitURL, sha))
	if checkRepoSha(dest, sha) {
		return dest, nil
	}
	// Fresh clone — wipe partial state if any.
	_ = os.RemoveAll(dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}

	// Try the cheap path first: shallow clone of the resolved sha. Some hosts
	// reject fetching a raw sha unless uploadpack.allowAnySHA1InWant=true; fall
	// back to a full clone + checkout if that fails.
	if err := shallowCloneSha(gitURL, sha, dest); err == nil {
		return dest, nil
	}
	return fullCloneCheckout(gitURL, sha, dest)
}

func shallowCloneSha(gitURL, sha, dest string) error {
	if err := runGit("", "init", "-q", dest); err != nil {
		return err
	}
	if err := runGit(dest, "remote", "add", "origin", gitURL); err != nil {
		return err
	}
	if err := runGit(dest, "fetch", "--depth=1", "origin", sha); err != nil {
		return err
	}
	return runGit(dest, "checkout", "-q", sha)
}

func fullCloneCheckout(gitURL, sha, dest string) (string, error) {
	if err := runGit("", "clone", "-q", gitURL, dest); err != nil {
		return "", err
	}
	if err := runGit(dest, "checkout", "-q", sha); err != nil {
		_ = os.RemoveAll(dest)
		return "", err
	}
	return dest, nil
}

func checkRepoSha(dir, expectedSha string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(string(out)), expectedSha)
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
