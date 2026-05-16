package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/axgrid/deploy/internal/protocol"
)

const defaultFileMode os.FileMode = 0o644

// runCtx is shared across tasks for a single Run invocation.
type runCtx struct {
	state   *State
	changed map[string]bool // paths written this run, keyed by absolute dst path
	dryRun  bool            // when true, handlers report would_change but don't apply
}

func newRunCtx(s *State, dryRun bool) *runCtx {
	return &runCtx{state: s, changed: map[string]bool{}, dryRun: dryRun}
}

func runCommandTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if ctx.dryRun {
		return protocol.Event{
			Status:  protocol.StatusWouldChange,
			Message: "would run: " + t.Command,
		}
	}
	cmd := exec.Command("/bin/sh", "-c", t.Command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exit := 0
	status := protocol.StatusChanged
	var msg string
	if err != nil {
		status = protocol.StatusError
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
			msg = err.Error()
		}
	}
	return protocol.Event{
		Status:   status,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exit,
		Message:  msg,
	}
}

// runFileTask handles copy and template tasks identically — both deliver an
// inline body that we materialize onto the remote path.
func runFileTask(ctx *runCtx, t protocol.Task) protocol.Event {
	if t.DstPath == "" {
		return protocol.Event{Status: protocol.StatusError, Message: "dst_path is empty"}
	}
	body, err := base64.StdEncoding.DecodeString(t.BodyB64)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "decode body: " + err.Error()}
	}
	// Defensive: verify the CLI-sent sha256 actually matches the decoded body.
	// Catches truncation / corruption before we touch the filesystem.
	if t.Sha256 != "" {
		actual := sha256Hex(body)
		if actual != t.Sha256 {
			return protocol.Event{
				Status:  protocol.StatusError,
				Path:    t.DstPath,
				Message: fmt.Sprintf("body sha256 mismatch: plan=%s got=%s", t.Sha256, actual),
			}
		}
	}

	mode := defaultFileMode
	modeStr := t.Mode
	if modeStr == "" {
		modeStr = fmt.Sprintf("%04o", defaultFileMode)
	} else {
		v, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "bad mode: " + modeStr}
		}
		mode = os.FileMode(v)
	}

	// Read current on-disk state to decide skip-vs-write. We compare both
	// content (sha256) AND mode so a `chmod` outside our system still triggers
	// a re-apply.
	currentSha, currentMode, exists, err := inspect(t.DstPath)
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "stat: " + err.Error()}
	}
	desiredSha := t.Sha256
	if desiredSha == "" {
		desiredSha = sha256Hex(body)
	}

	if exists && currentSha == desiredSha && currentMode == mode {
		// Nothing to do; refresh the state row so updated_at advances. In
		// dry-run we don't mutate state, but the answer is still "skipped".
		if !ctx.dryRun {
			ctx.state.Files[t.DstPath] = &FileState{
				Sha256:    desiredSha,
				Mode:      modeStr,
				AppliedAt: ctx.state.Files[t.DstPath].appliedAtOrNow(),
			}
		}
		return protocol.Event{Status: protocol.StatusSkipped, Path: t.DstPath}
	}

	if ctx.dryRun {
		// Mark this path as "would have changed" so a follow-up command with
		// when_changed shows up in the preview too.
		ctx.changed[t.DstPath] = true
		msg := "would write"
		if exists {
			msg = "would overwrite"
		}
		return protocol.Event{Status: protocol.StatusWouldChange, Path: t.DstPath, Message: msg}
	}

	if err := os.MkdirAll(filepath.Dir(t.DstPath), 0o755); err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "mkdir parent: " + err.Error()}
	}
	if err := writeAtomic(t.DstPath, body, mode); err != nil {
		return protocol.Event{Status: protocol.StatusError, Path: t.DstPath, Message: "write: " + err.Error()}
	}

	ctx.changed[t.DstPath] = true
	ctx.state.Files[t.DstPath] = &FileState{
		Sha256:    desiredSha,
		Mode:      modeStr,
		AppliedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return protocol.Event{Status: protocol.StatusChanged, Path: t.DstPath}
}

func inspect(path string) (sha string, mode os.FileMode, exists bool, err error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, false, err
	}
	return hex.EncodeToString(h.Sum(nil)), st.Mode().Perm(), true, nil
}

// writeAtomic writes to a tmp sibling and renames, so readers never see a
// partial file. Mode is applied with chmod after creation to bypass umask.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".deploy.tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (f *FileState) appliedAtOrNow() string {
	if f != nil && f.AppliedAt != "" {
		return f.AppliedAt
	}
	return time.Now().UTC().Format(time.RFC3339)
}
