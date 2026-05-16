package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is persisted to ~/.deploy-state/<rulebook>/state.json on the remote.
// It records what the agent has applied so it can skip unchanged files on the
// next run.
type State struct {
	RulebookName string                `json:"rulebook_name"`
	UpdatedAt    string                `json:"updated_at"`
	Files        map[string]*FileState `json:"files"`
}

type FileState struct {
	Sha256    string `json:"sha256"`
	Mode      string `json:"mode"`
	AppliedAt string `json:"applied_at"`
}

func statePath(rulebook string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".deploy-state", rulebook, "state.json"), nil
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
