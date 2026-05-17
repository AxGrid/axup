package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/axgrid/axup/internal/protocol"
)

// runChmodTask sets POSIX mode on an existing path. Idempotent — stat
// first, only chmod if perm differs.
//
// Refuses to chmod a path that doesn't exist (error). Symlinks are
// traversed (Stat, not Lstat) because chmod on a symlink targets the
// thing it points at on Linux anyway — that's the typical mental model.
func runChmodTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.DstPath == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "chmod: empty path"}
	}
	mode, err := parseModeOr(t.Mode, 0)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "bad mode: " + err.Error()}
	}
	if t.Mode == "" {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chmod: mode is required"}
	}
	st, err := os.Stat(t.DstPath)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chmod: path does not exist"}
	}
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "stat: " + err.Error()}
	}
	cur := st.Mode().Perm()
	if cur == mode {
		return protocol.Event{Status: protocol.StatusSkipped, Path: t.DstPath, Message: fmt.Sprintf("mode already %04o", mode)}
	}
	if ctx.dryRun {
		ctx.changed[t.DstPath] = true
		return protocol.Event{Status: protocol.StatusWouldChange, Path: t.DstPath, Message: fmt.Sprintf("would chmod %04o → %04o", cur, mode)}
	}
	if err := os.Chmod(t.DstPath, mode); err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chmod: " + err.Error()}
	}
	ctx.changed[t.DstPath] = true
	return protocol.Event{Status: protocol.StatusChanged, Path: t.DstPath, Message: fmt.Sprintf("chmod %04o → %04o", cur, mode)}
}

// runChownTask sets owner and/or group on an existing path. At least one
// of owner / group must be set (validated at parse time).
//
// Idempotency: check the TOP path's current uid/gid. If both match the
// requested values, skip. Otherwise chown (or chown -R when Recursive).
//
// For recursive runs, we trust the top path to represent the tree's
// state — if the top is right, the children are assumed right too. This
// is the pragmatic ~95% case. Edge case: a manual chown by another
// admin on some children won't trigger a re-run; force it by chowning
// the top to a different user first, then back.
func runChownTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.DstPath == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "chown: empty path"}
	}
	uid, gid, err := resolveOwnership(t.ChownOwner, t.ChownGroup)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: err.Error()}
	}
	st, err := os.Lstat(t.DstPath)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chown: path does not exist"}
	}
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "lstat: " + err.Error()}
	}
	curUID, curGID := statOwner(st)
	needsOwner := uid != -1 && curUID != uid
	needsGroup := gid != -1 && curGID != gid
	if !needsOwner && !needsGroup {
		return protocol.Event{Status: protocol.StatusSkipped, Path: t.DstPath, Message: ownershipDescr(curUID, curGID) + " already"}
	}
	if ctx.dryRun {
		ctx.changed[t.DstPath] = true
		scope := ""
		if t.ChownRecursive {
			scope = " (-R)"
		}
		return protocol.Event{Status: protocol.StatusWouldChange, Path: t.DstPath, Message: fmt.Sprintf("would chown%s %d/%d → %d/%d", scope, curUID, curGID, ifMinusOne(uid, curUID), ifMinusOne(gid, curGID))}
	}
	if t.ChownRecursive {
		// Walk the tree. Lchown so we don't dereference symlinks (matches
		// `chown -RP` — owner of the symlink itself, not its target).
		err := filepath.Walk(t.DstPath, func(path string, _ os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			return os.Lchown(path, ifMinusOne(uid, -1), ifMinusOne(gid, -1))
		})
		if err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chown -R: " + err.Error()}
		}
	} else {
		if err := os.Lchown(t.DstPath, ifMinusOne(uid, -1), ifMinusOne(gid, -1)); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "chown: " + err.Error()}
		}
	}
	ctx.changed[t.DstPath] = true
	scope := ""
	if t.ChownRecursive {
		scope = " -R"
	}
	return protocol.Event{Status: protocol.StatusChanged, Path: t.DstPath, Message: fmt.Sprintf("chown%s %d/%d → %d/%d", scope, curUID, curGID, ifMinusOne(uid, curUID), ifMinusOne(gid, curGID))}
}

// ifMinusOne returns `fallback` when v == -1, else v. Used to keep
// "don't touch this id" semantics through Lchown while still emitting
// a human-friendly "was/now" string in the message.
func ifMinusOne(v, fallback int) int {
	if v == -1 {
		return fallback
	}
	return v
}

func ownershipDescr(uid, gid int) string {
	return fmt.Sprintf("uid=%d gid=%d", uid, gid)
}
