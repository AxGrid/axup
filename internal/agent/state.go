package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// State is persisted to ~/.axup-state/<rulebook>/state.json on the remote.
// It records what the agent has applied so it can skip unchanged files on the
// next run.
type State struct {
	RulebookName string                `json:"rulebook_name"`
	UpdatedAt    string                `json:"updated_at"`
	Files        map[string]*FileState `json:"files"`
}

type FileState struct {
	Sha256    string         `json:"sha256"`
	Mode      string         `json:"mode"`
	AppliedAt string         `json:"applied_at"`
	History   []HistoryEntry `json:"history,omitempty"` // newest first; populated when Plan.KeepHistory>0 and the file is overwritten
}

// HistoryEntry records one previous version of a tracked file. ArchivedPath
// points to a copy of the file body kept under ~/.axup-state/<rulebook>/history/,
// which `axup rollback` reads back to restore the file. Phase + TaskID are
// informational — they let `axup status --history` surface which deploy
// produced each version.
type HistoryEntry struct {
	Sha256       string `json:"sha256"`
	Mode         string `json:"mode"`
	ArchivedPath string `json:"archived_path"`
	RecordedAt   string `json:"recorded_at"`
	Phase        string `json:"phase,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
}

func statePath(rulebook string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".axup-state", rulebook, "state.json"), nil
}

// historyDir returns ~/.axup-state/<rulebook>/history/, creating it (0700)
// if missing. Archives are written here on overwrite and read back on
// rollback; the directory is also the unit of cleanup for `axup history clear`.
func historyDir(rulebook string) (string, error) {
	p, err := statePath(rulebook)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(p), "history")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// archiveCurrentFile copies srcPath into the rulebook's history dir under a
// random filename and returns the absolute archived path. Archive permission
// is forced to 0600 — these are backups, restore-on-rollback re-applies the
// original mode from HistoryEntry.Mode. Returns ("", nil) if srcPath does
// not exist (fresh write, nothing to archive).
func archiveCurrentFile(rulebook, srcPath string) (string, error) {
	src, err := os.Open(srcPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer src.Close()
	dir, err := historyDir(rulebook)
	if err != nil {
		return "", err
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	archived := filepath.Join(dir, hex.EncodeToString(suffix)+".bak")
	dst, err := os.OpenFile(archived, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(archived)
		return "", err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(archived)
		return "", err
	}
	return archived, nil
}

// pushHistory prepends entry and evicts past `keep`, best-effort rm'ing the
// archived files of any evicted entries. The slice is updated in place via
// the returned value.
func pushHistory(history []HistoryEntry, entry HistoryEntry, keep int) []HistoryEntry {
	out := append([]HistoryEntry{entry}, history...)
	if keep > 0 && len(out) > keep {
		for _, evicted := range out[keep:] {
			_ = os.Remove(evicted.ArchivedPath)
		}
		out = out[:keep]
	}
	return out
}

// clearAllHistory drops every FileState.History on `s` and removes the
// rulebook's history dir. Returns the number of entries dropped (across
// all files) so the caller can report it back to the user. Best-effort
// rm of the dir — if it fails the state.json still records the cleared
// History fields, so the next deploy starts fresh.
func (s *State) clearAllHistory() (int, error) {
	dropped := 0
	for _, fs := range s.Files {
		dropped += len(fs.History)
		fs.History = nil
	}
	p, err := statePath(s.RulebookName)
	if err != nil {
		return dropped, err
	}
	dir := filepath.Join(filepath.Dir(p), "history")
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return dropped, err
	}
	return dropped, nil
}

func loadState(rulebook string) (*State, error) {
	p, err := statePath(rulebook)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &State{RulebookName: rulebook, Files: map[string]*FileState{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if s.Files == nil {
		s.Files = map[string]*FileState{}
	}
	return &s, nil
}

// save writes the state atomically (tmp + rename) so a crash mid-write can't
// corrupt the file.
func (s *State) save() error {
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	p, err := statePath(s.RulebookName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
