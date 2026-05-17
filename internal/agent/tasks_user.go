package agent

import (
	"bytes"
	"errors"
	"os/exec"
	"os/user"
	"sort"
	"strings"

	"github.com/axgrid/axup/internal/protocol"
)

// runUserTask creates or deletes a system user. Scope is intentionally
// minimal — we do NOT reconcile shell / home / uid on an existing user
// (changing those is rare in deploy-tool land and the failure modes are
// nasty). Supplementary groups ARE reconciled because adding a new group
// is a common ongoing need.
//
// Idempotency:
//   - state=present:
//       * absent → useradd -r [--shell] [--home-dir] [--create-home] → changed
//       * present + groups subset matches → skipped
//       * present + groups need additions → usermod -aG → changed
//   - state=absent:
//       * present → userdel -r (and ignore EXIT 6 "user does not exist")
//       * absent → skipped
//
// All operations shell out to the canonical Debian/Ubuntu tools
// (useradd / userdel / usermod), which are present on every supported
// distro. Alpine / BusyBox would need adduser/deluser — out of scope.
func runUserTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.UserName == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "user: empty name"}
	}
	state := t.UserState
	if state == "" {
		state = "present"
	}

	exists, err := userExists(t.UserName)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Message: "lookup user: " + err.Error()}
	}

	if state == "absent" {
		if !exists {
			return protocol.Event{Status: protocol.StatusSkipped, Message: "user " + t.UserName + " already absent"}
		}
		if ctx.dryRun {
			return protocol.Event{Status: protocol.StatusWouldChange, Message: "would userdel -r " + t.UserName}
		}
		if out, err := runCmd("userdel", "-r", t.UserName); err != nil {
			// userdel exit 6 = user doesn't exist (race with another caller); treat as success.
			if !strings.Contains(out, "does not exist") {
				return protocol.Event{Status: protocol.StatusError, Message: "userdel: " + err.Error() + ": " + out}
			}
		}
		return protocol.Event{Status: protocol.StatusChanged, Message: "deleted user " + t.UserName}
	}

	// state == present
	if !exists {
		args := []string{"-r"} // always system user
		if t.UserShell != "" {
			args = append(args, "--shell", t.UserShell)
		} else {
			args = append(args, "--shell", "/usr/sbin/nologin")
		}
		if t.UserHome != "" {
			args = append(args, "--home-dir", t.UserHome)
		}
		if t.UserCreateHome {
			args = append(args, "--create-home")
		} else {
			args = append(args, "--no-create-home")
		}
		if len(t.UserGroups) > 0 {
			args = append(args, "--groups", strings.Join(t.UserGroups, ","))
		}
		args = append(args, t.UserName)
		if ctx.dryRun {
			return protocol.Event{Status: protocol.StatusWouldChange, Message: "would useradd " + strings.Join(args, " ")}
		}
		if out, err := runCmd("useradd", args...); err != nil {
			return protocol.Event{Status: protocol.StatusError, Message: "useradd: " + err.Error() + ": " + out}
		}
		return protocol.Event{Status: protocol.StatusChanged, Message: "created user " + t.UserName}
	}

	// User exists — reconcile supplementary groups only. We compare
	// requested vs current and add missing ones via `usermod -aG`. We do
	// NOT remove a user from a group that's no longer listed — removing
	// group membership is destructive enough that the user should `usermod
	// -G` manually if they want strict set-semantics.
	if len(t.UserGroups) == 0 {
		return protocol.Event{Status: protocol.StatusSkipped, Message: "user " + t.UserName + " already present"}
	}
	current, err := userGroups(t.UserName)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Message: "current groups: " + err.Error()}
	}
	missing := diffGroups(t.UserGroups, current)
	if len(missing) == 0 {
		return protocol.Event{Status: protocol.StatusSkipped, Message: "user " + t.UserName + " already in groups " + strings.Join(t.UserGroups, ",")}
	}
	if ctx.dryRun {
		return protocol.Event{Status: protocol.StatusWouldChange, Message: "would usermod -aG " + strings.Join(missing, ",") + " " + t.UserName}
	}
	if out, err := runCmd("usermod", "-aG", strings.Join(missing, ","), t.UserName); err != nil {
		return protocol.Event{Status: protocol.StatusError, Message: "usermod: " + err.Error() + ": " + out}
	}
	return protocol.Event{Status: protocol.StatusChanged, Message: "added user " + t.UserName + " to groups " + strings.Join(missing, ",")}
}

// userExists reports whether `name` is in /etc/passwd. Wraps os/user with a
// known-empty fallback for the "user.UnknownUserError" case.
func userExists(name string) (bool, error) {
	_, err := user.Lookup(name)
	if err == nil {
		return true, nil
	}
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return false, nil
	}
	return false, err
}

// userGroups returns the supplementary group names of `name`. Uses os/user
// to map gids back to names so we can compare against the rulebook's
// human-readable group list without parsing /etc/group ourselves.
func userGroups(name string) ([]string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	gids, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(gids))
	for _, gid := range gids {
		g, err := user.LookupGroupId(gid)
		if err != nil {
			continue // missing /etc/group entry — skip
		}
		out = append(out, g.Name)
	}
	sort.Strings(out)
	return out, nil
}

// diffGroups returns wanted - have (set difference). Used to figure out
// which groups need to be added via `usermod -aG`.
func diffGroups(wanted, have []string) []string {
	haveSet := make(map[string]bool, len(have))
	for _, g := range have {
		haveSet[g] = true
	}
	var missing []string
	for _, g := range wanted {
		if !haveSet[g] {
			missing = append(missing, g)
		}
	}
	return missing
}

// runCmd is a thin wrapper around exec.Command that captures combined
// output for error messages. Stdin is empty; PATH is inherited.
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

