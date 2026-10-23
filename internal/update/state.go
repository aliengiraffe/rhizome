package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"rhizome/internal/localstate"
)

// StateFile is the private status record under the runtime directory.
const StateFile = "update-state.json"

// maxState bounds the state file read.
const maxState = 64 << 10

// Pending records an installed but unconfirmed update.
type Pending struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Target    string    `json:"target"`
	Backup    string    `json:"backup"`
	StartedAt time.Time `json:"started_at"`
}

// State is what `rhizome service status` and the terminal view show. Values
// are version strings, paths and outcome codes only, never payloads.
type State struct {
	Version     string    `json:"version"`
	Target      string    `json:"target,omitempty"`
	LastCheckAt time.Time `json:"last_check_at,omitempty"`
	LastOutcome string    `json:"last_outcome,omitempty"`
	LastDetail  string    `json:"last_detail,omitempty"`
	Available   string    `json:"available_version,omitempty"`
	Blocked     string    `json:"blocked,omitempty"`
	Failures    int       `json:"consecutive_failures"`
	NextCheckAt time.Time `json:"next_check_at,omitempty"`
	Pending     *Pending  `json:"pending,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ReadState loads the state file; a missing file yields the zero State.
func ReadState(runtime string) (State, error) {
	var s State
	f, err := os.Open(filepath.Join(runtime, StateFile))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxState+1))
	if err != nil {
		return s, err
	}
	if len(b) > maxState {
		return s, errors.New("update state too large")
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("update state: %w", err)
	}
	return s, nil
}

// writeState replaces the state file atomically with private permissions.
func writeState(runtime string, s State) error {
	if err := localstate.SecureDir(runtime); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(runtime, ".update-state-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(runtime, StateFile))
}
