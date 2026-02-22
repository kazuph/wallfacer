package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// saveTask atomically writes a task's metadata to its task.json file.
// Must be called with s.mu held for writing.
func (s *Store) saveTask(id uuid.UUID, task *Task) error {
	path := filepath.Join(s.dir, id.String(), "task.json")
	return atomicWriteJSON(path, task)
}

// SaveTurnOutput persists raw stdout/stderr for a given turn to the outputs directory.
func (s *Store) SaveTurnOutput(taskID uuid.UUID, turn int, stdout, stderr []byte) error {
	outputsDir := filepath.Join(s.dir, taskID.String(), "outputs")
	if err := os.MkdirAll(outputsDir, 0700); err != nil {
		return fmt.Errorf("create outputs dir: %w", err)
	}

	name := fmt.Sprintf("turn-%04d.json", turn)
	if err := os.WriteFile(filepath.Join(outputsDir, name), stdout, 0600); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}

	if len(stderr) > 0 {
		stderrName := fmt.Sprintf("turn-%04d.stderr.txt", turn)
		if err := os.WriteFile(filepath.Join(outputsDir, stderrName), stderr, 0600); err != nil {
			return fmt.Errorf("write stderr: %w", err)
		}
	}

	return nil
}

// atomicWriteJSON marshals v to JSON and writes it atomically via temp+rename.
func atomicWriteJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// jsonUnmarshal is a thin wrapper around json.Unmarshal used internally.
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
