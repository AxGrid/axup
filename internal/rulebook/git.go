package rulebook

import (
	"bytes"
	"os/exec"
	"strings"
)

// gitVars returns auto-discovered git facts for the given working directory.
// Each value is "" when git is unavailable or the directory isn't a repo —
// callers can still reference them in templates without errors.
//
// Keys produced:
//   - git_sha       — full HEAD sha (40 chars)
//   - git_short_sha — first 7 chars of HEAD sha
//   - git_branch    — current branch name (empty when detached HEAD)
//   - git_dirty     — "true" if working tree has uncommitted changes, else "false"
func gitVars(dir string) map[string]any {
	out := map[string]any{
		"git_sha":       "",
		"git_short_sha": "",
		"git_branch":    "",
		"git_dirty":     "false",
	}
	if _, err := exec.LookPath("git"); err != nil {
		return out
	}
	if sha := gitCmd(dir, "rev-parse", "HEAD"); sha != "" {
		out["git_sha"] = sha
		if len(sha) >= 7 {
			out["git_short_sha"] = sha[:7]
		}
	}
	if branch := gitCmd(dir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "" && branch != "HEAD" {
		out["git_branch"] = branch
	}
	if status := gitCmd(dir, "status", "--porcelain"); status != "" {
		out["git_dirty"] = "true"
	}
	return out
}

func gitCmd(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}
