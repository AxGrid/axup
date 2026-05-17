package agent

import (
	"errors"
	"os/user"
	"strings"

	"github.com/axgrid/axup/internal/protocol"
)

// runGroupTask creates or deletes a system group. Mirrors runUserTask's
// minimal scope: exists-or-not, no gid pinning, no rename.
//
// Idempotency:
//   - state=present + absent → groupadd -r → changed
//   - state=present + present → skipped
//   - state=absent + present → groupdel
//   - state=absent + absent → skipped
//
// Always uses `-r` (system group). For non-system groups, drop down to
// `command:`.
func runGroupTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.GroupName == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "group: empty name"}
	}
	state := t.GroupState
	if state == "" {
		state = "present"
	}
	exists, err := groupExists(t.GroupName)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Message: "lookup group: " + err.Error()}
	}
	if state == "absent" {
		if !exists {
			return protocol.Event{Status: protocol.StatusSkipped, Message: "group " + t.GroupName + " already absent"}
		}
		if ctx.dryRun {
			return protocol.Event{Status: protocol.StatusWouldChange, Message: "would groupdel " + t.GroupName}
		}
		if out, err := runCmd("groupdel", t.GroupName); err != nil {
			if !strings.Contains(out, "does not exist") {
				return protocol.Event{Status: protocol.StatusError, Message: "groupdel: " + err.Error() + ": " + out}
			}
		}
		return protocol.Event{Status: protocol.StatusChanged, Message: "deleted group " + t.GroupName}
	}
	// state == present
	if exists {
		return protocol.Event{Status: protocol.StatusSkipped, Message: "group " + t.GroupName + " already present"}
	}
	if ctx.dryRun {
		return protocol.Event{Status: protocol.StatusWouldChange, Message: "would groupadd -r " + t.GroupName}
	}
	if out, err := runCmd("groupadd", "-r", t.GroupName); err != nil {
		return protocol.Event{Status: protocol.StatusError, Message: "groupadd: " + err.Error() + ": " + out}
	}
	return protocol.Event{Status: protocol.StatusChanged, Message: "created group " + t.GroupName}
}

func groupExists(name string) (bool, error) {
	_, err := user.LookupGroup(name)
	if err == nil {
		return true, nil
	}
	var unknown user.UnknownGroupError
	if errors.As(err, &unknown) {
		return false, nil
	}
	return false, err
}
