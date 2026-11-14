package audio

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// releaseState is everything Restore needs to put a released device's audio
// back. It is kept on disk so a crash or restart can still restore it.
type releaseState struct {
	Address        string            `json:"address"`
	Card           string            `json:"card"`
	Profile        string            `json:"profile"`
	DefaultSink    string            `json:"default_sink"`
	ConfiguredSink string            `json:"configured_sink"`
	ConfiguredSet  bool              `json:"configured_set"`
	Module         string            `json:"module"`
	Streams        map[string]string `json:"streams"` // sink-input index -> sink name
}

const statePattern = "release-*.json"

func stateFile(dir, addrUnderscore string) string {
	return filepath.Join(dir, "release-"+addrUnderscore+".json")
}

// writeState writes st atomically to path, creating the directory.
func writeState(path string, st *releaseState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("audio: state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".release-*.tmp")
	if err != nil {
		return fmt.Errorf("audio: write state: %w", err)
	}
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Sync(), tmp.Close()); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("audio: write state: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("audio: write state: %w", err)
	}
	return nil
}

func readState(path string) (*releaseState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st releaseState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("audio: parse %s: %w", path, err)
	}
	return &st, nil
}

func removeState(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("audio: remove state: %w", err)
	}
	return nil
}
