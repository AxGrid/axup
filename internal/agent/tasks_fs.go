package agent

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/axgrid/axup/internal/protocol"
)

// statOwner extracts numeric uid/gid from a POSIX FileInfo. Works on both
// Linux (the agent's GOOS) and Darwin (where the CLI compiles the agent
// package for type-checking even though only Linux binaries get embedded).
func statOwner(st os.FileInfo) (uid int, gid int) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid), int(sys.Gid)
	}
	return -1, -1
}

// runMkdirTask creates Plan.MkdirPath (with parents) and reconciles mode +
// optional owner/group. Always uses MkdirAll semantics — partial paths
// (parent missing) are not an error.
//
// Idempotency:
//   - absent → MkdirAll + chmod + optional chown → changed
//   - present + matches mode/owner/group → skipped
//   - present + drift on any of mode/owner/group → chmod / chown → changed
//   - present and is a regular file → error (we never auto-rm to "fix" it)
func runMkdirTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.MkdirPath == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "mkdir: empty path"}
	}
	mode, err := parseModeOr(t.Mode, 0o755)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: "bad mode: " + err.Error()}
	}

	uid, gid, ownErr := resolveOwnership(t.MkdirOwner, t.MkdirGroup)
	if ownErr != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: ownErr.Error()}
	}

	st, err := os.Stat(t.MkdirPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if ctx.dryRun {
			ctx.changed[t.MkdirPath] = true
			return protocol.Event{Status: protocol.StatusWouldChange, Path: t.MkdirPath, Message: fmt.Sprintf("would mkdir -p (mode=%04o)", mode)}
		}
		if err := os.MkdirAll(t.MkdirPath, mode); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: "mkdir: " + err.Error()}
		}
		// MkdirAll applies umask — re-chmod to be deterministic.
		if err := os.Chmod(t.MkdirPath, mode); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: "chmod: " + err.Error()}
		}
		if uid != -1 || gid != -1 {
			if err := os.Chown(t.MkdirPath, uid, gid); err != nil {
				return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: "chown: " + err.Error()}
			}
		}
		ctx.changed[t.MkdirPath] = true
		return protocol.Event{Status: protocol.StatusChanged, Path: t.MkdirPath, Message: fmt.Sprintf("created (mode=%04o)", mode)}

	case err != nil:
		return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: "stat: " + err.Error()}

	case !st.IsDir():
		return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: "exists but is not a directory (refusing to auto-rm)"}

	default:
		// Dir exists — reconcile mode + ownership.
		curMode := st.Mode().Perm()
		curUID, curGID := statOwner(st)
		needChmod := curMode != mode
		needChown := (uid != -1 && curUID != uid) || (gid != -1 && curGID != gid)
		if !needChmod && !needChown {
			return protocol.Event{Status: protocol.StatusSkipped, Path: t.MkdirPath}
		}
		if ctx.dryRun {
			ctx.changed[t.MkdirPath] = true
			var why string
			switch {
			case needChmod && needChown:
				why = fmt.Sprintf("would chmod %04o→%04o + chown uid=%d gid=%d", curMode, mode, uid, gid)
			case needChmod:
				why = fmt.Sprintf("would chmod %04o→%04o", curMode, mode)
			default:
				why = fmt.Sprintf("would chown uid=%d gid=%d (was %d/%d)", uid, gid, curUID, curGID)
			}
			return protocol.Event{Status: protocol.StatusWouldChange, Path: t.MkdirPath, Message: why}
		}
		if needChmod {
			if err := os.Chmod(t.MkdirPath, mode); err != nil {
				return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: "chmod: " + err.Error()}
			}
		}
		if needChown {
			if err := os.Chown(t.MkdirPath, uid, gid); err != nil {
				return protocol.Event{Status: protocol.StatusError, Path: t.MkdirPath, Message: "chown: " + err.Error()}
			}
		}
		ctx.changed[t.MkdirPath] = true
		return protocol.Event{Status: protocol.StatusChanged, Path: t.MkdirPath, Message: "reconciled mode/ownership"}
	}
}

// runSymlinkTask creates or updates a symlink at Plan.SymlinkDst pointing
// to Plan.SymlinkSrc.
//
// Idempotency: when Dst is already a symlink AND its target matches Src, skip.
// Otherwise: with Force, unlink whatever's at Dst and create the symlink;
// without Force, refuse to clobber a regular file/directory.
func runSymlinkTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.SymlinkSrc == "" || t.SymlinkDst == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "symlink: src and dst are required"}
	}
	// Lstat so we inspect the symlink itself, not what it points to.
	st, err := os.Lstat(t.SymlinkDst)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if ctx.dryRun {
			ctx.changed[t.SymlinkDst] = true
			return protocol.Event{Status: protocol.StatusWouldChange, Path: t.SymlinkDst, Message: fmt.Sprintf("would create symlink → %s", t.SymlinkSrc)}
		}
		if err := os.MkdirAll(filepath.Dir(t.SymlinkDst), 0o755); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "mkdir parent: " + err.Error()}
		}
		if err := os.Symlink(t.SymlinkSrc, t.SymlinkDst); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "symlink: " + err.Error()}
		}
		ctx.changed[t.SymlinkDst] = true
		return protocol.Event{Status: protocol.StatusChanged, Path: t.SymlinkDst, Message: fmt.Sprintf("symlink → %s", t.SymlinkSrc)}

	case err != nil:
		return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "lstat: " + err.Error()}

	case st.Mode()&os.ModeSymlink != 0:
		// Dst is a symlink already — compare targets.
		curTarget, err := os.Readlink(t.SymlinkDst)
		if err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "readlink: " + err.Error()}
		}
		if curTarget == t.SymlinkSrc {
			return protocol.Event{Status: protocol.StatusSkipped, Path: t.SymlinkDst}
		}
		if ctx.dryRun {
			ctx.changed[t.SymlinkDst] = true
			return protocol.Event{Status: protocol.StatusWouldChange, Path: t.SymlinkDst, Message: fmt.Sprintf("would relink %s → %s (was %s)", t.SymlinkDst, t.SymlinkSrc, curTarget)}
		}
		if err := os.Remove(t.SymlinkDst); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "unlink old: " + err.Error()}
		}
		if err := os.Symlink(t.SymlinkSrc, t.SymlinkDst); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "relink: " + err.Error()}
		}
		ctx.changed[t.SymlinkDst] = true
		return protocol.Event{Status: protocol.StatusChanged, Path: t.SymlinkDst, Message: fmt.Sprintf("relinked → %s (was %s)", t.SymlinkSrc, curTarget)}

	default:
		// Dst is a regular file or directory — only clobber with Force.
		if !t.SymlinkForce {
			return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "exists and is not a symlink — pass `force: true` to replace"}
		}
		if ctx.dryRun {
			ctx.changed[t.SymlinkDst] = true
			return protocol.Event{Status: protocol.StatusWouldChange, Path: t.SymlinkDst, Message: fmt.Sprintf("would replace existing %s with symlink → %s", st.Mode().Type().String(), t.SymlinkSrc)}
		}
		// RemoveAll handles both regular files and directories. A user
		// passing Force on a directory tree is explicitly opting in.
		if err := os.RemoveAll(t.SymlinkDst); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "remove existing: " + err.Error()}
		}
		if err := os.Symlink(t.SymlinkSrc, t.SymlinkDst); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.SymlinkDst, Message: "symlink: " + err.Error()}
		}
		ctx.changed[t.SymlinkDst] = true
		return protocol.Event{Status: protocol.StatusChanged, Path: t.SymlinkDst, Message: fmt.Sprintf("replaced existing → symlink → %s", t.SymlinkSrc)}
	}
}

// runRemoveTask deletes Plan.RemovePath. Symlinks are unlinked (target left
// alone). Non-empty directories require Plan.RemoveRecursive — without it
// os.Remove returns ENOTEMPTY, surfaced as an error.
//
// Idempotency: absent → skipped.
func runRemoveTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.RemovePath == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "remove: empty path"}
	}
	st, err := os.Lstat(t.RemovePath)
	if errors.Is(err, os.ErrNotExist) {
		return protocol.Event{Status: protocol.StatusSkipped, Path: t.RemovePath, Message: "already absent"}
	}
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.RemovePath, Message: "lstat: " + err.Error()}
	}
	if ctx.dryRun {
		ctx.changed[t.RemovePath] = true
		return protocol.Event{Status: protocol.StatusWouldChange, Path: t.RemovePath, Message: fmt.Sprintf("would remove %s", describeFile(st))}
	}
	// Symlinks: always use Remove (we never want to follow into the target).
	// Directories: pick Remove vs RemoveAll based on Recursive.
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() || !t.RemoveRecursive {
		if err := os.Remove(t.RemovePath); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.RemovePath, Message: "remove: " + err.Error()}
		}
	} else {
		if err := os.RemoveAll(t.RemovePath); err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.RemovePath, Message: "remove -r: " + err.Error()}
		}
	}
	ctx.changed[t.RemovePath] = true
	return protocol.Event{Status: protocol.StatusChanged, Path: t.RemovePath, Message: fmt.Sprintf("removed %s", describeFile(st))}
}

// describeFile returns a short noun for log lines: "file" / "dir" / "symlink".
// Helps disambiguate "removed dir /opt/foo" vs "removed file /opt/foo".
func describeFile(st os.FileInfo) string {
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		return "symlink"
	case st.IsDir():
		return "dir"
	default:
		return "file"
	}
}

// parseModeOr parses an octal mode string (e.g. "0755") with a fallback when
// the string is empty. Mirrors the pattern in runFileTask but lets callers
// pick their own default.
func parseModeOr(modeStr string, fallback os.FileMode) (os.FileMode, error) {
	if modeStr == "" {
		return fallback, nil
	}
	v, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return 0, err
	}
	return os.FileMode(v), nil
}

// resolveOwnership turns username/group strings into numeric uid/gid via
// /etc/passwd + /etc/group (os/user). Returns (-1, -1, nil) when neither was
// requested. Failure to resolve a non-empty name returns an error so the
// agent surfaces "user app does not exist" instead of silently chowning to
// uid -1.
func resolveOwnership(owner, group string) (uid int, gid int, err error) {
	uid, gid = -1, -1
	if owner != "" {
		u, err := user.Lookup(owner)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve owner %q: %w (run earlier task to useradd it, or drop the field)", owner, err)
		}
		uid, err = strconv.Atoi(u.Uid)
		if err != nil {
			return 0, 0, fmt.Errorf("parse uid for %q: %w", owner, err)
		}
	}
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve group %q: %w", group, err)
		}
		gid, err = strconv.Atoi(g.Gid)
		if err != nil {
			return 0, 0, fmt.Errorf("parse gid for %q: %w", group, err)
		}
	}
	return uid, gid, nil
}
