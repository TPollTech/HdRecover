package recovery

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const sessionFileName = ".hdrecover-session.json"

func saveSession(path string, state SessionState) error {
	state.LastUpdate = time.Now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func loadSession(path string) (SessionState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionState{}, err
	}
	var state SessionState
	if err := json.Unmarshal(data, &state); err != nil {
		return SessionState{}, err
	}
	if state.Version <= 0 || state.SourcePath == "" {
		return SessionState{}, errors.New("sessão inválida")
	}
	return state, nil
}

// FindResumableSession returns the newest compatible interrupted session under destination.
func FindResumableSession(destination, sourcePath string, sourceSize, rangeStart, rangeSize int64, mode OperationMode, configSignature string) (SessionState, string, error) {
	entries, err := os.ReadDir(destination)
	if err != nil {
		return SessionState{}, "", err
	}
	type candidate struct {
		state SessionState
		path  string
	}
	matches := make([]candidate, 0)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "HdRecover_") {
			continue
		}
		path := filepath.Join(destination, entry.Name(), sessionFileName)
		state, err := loadSession(path)
		if err != nil || state.Completed {
			continue
		}
		if !strings.EqualFold(state.SourcePath, sourcePath) || state.SourceSize != sourceSize || state.Mode != mode {
			continue
		}
		if state.RangeStart != rangeStart || state.RangeSize != rangeSize {
			continue
		}
		if state.ConfigSignature != "" && configSignature != "" && state.ConfigSignature != configSignature {
			continue
		}
		matches = append(matches, candidate{state: state, path: path})
	}
	if len(matches) == 0 {
		return SessionState{}, "", os.ErrNotExist
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].state.LastUpdate.After(matches[j].state.LastUpdate) })
	return matches[0].state, matches[0].path, nil
}
