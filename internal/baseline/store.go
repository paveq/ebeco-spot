// Package baseline persists the per-device "on" baseline target temperature so
// a user's manual setpoint survives restarts and on/off cycling.
package baseline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store holds device-id -> baseline °C, backed by a JSON file.
type Store struct {
	path string
	mu   sync.Mutex
	data map[int]float64
}

// Load reads the store from path. A missing file yields an empty store.
func Load(path string) (*Store, error) {
	s := &Store{path: path, data: make(map[int]float64)}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading baseline store %q: %w", path, err)
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("parsing baseline store %q: %w", path, err)
		}
	}
	return s, nil
}

// Get returns the stored baseline for a device, if any.
func (s *Store) Get(deviceID int) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[deviceID]
	return v, ok
}

// Set records a baseline and flushes to disk. Writing the same value again is a
// no-op (no disk write).
func (s *Store) Set(deviceID int, baseline float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.data[deviceID]; ok && v == baseline {
		return nil
	}
	s.data[deviceID] = baseline
	return s.flushLocked()
}

// flushLocked writes the store atomically (temp file + rename).
func (s *Store) flushLocked() error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating state dir %q: %w", dir, err)
		}
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("writing baseline store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replacing baseline store: %w", err)
	}
	return nil
}
