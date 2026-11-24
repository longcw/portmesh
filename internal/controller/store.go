package controller

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/longcw/portmesh/internal/proto"
)

// Device is an enrolled agent; only the hash of its token is kept.
type Device struct {
	Name      string    `json:"name"`
	TokenHash string    `json:"token_hash"`
	Created   time.Time `json:"created"`
}

// State is the persisted configuration, the single source of truth for every forward.
type State struct {
	Devices  []Device        `json:"devices"`
	Forwards []proto.Forward `json:"forwards"`
}

func loadState(path string) (State, error) {
	var st State
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func saveState(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
