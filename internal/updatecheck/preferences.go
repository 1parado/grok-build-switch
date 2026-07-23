package updatecheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"grok_switch/internal/recovery"
)

type Preferences struct {
	NotifiedVersion string `json:"notified_version,omitempty"`
	SkippedVersion  string `json:"skipped_version,omitempty"`
}

type PreferenceStore struct {
	path string
	mu   sync.Mutex
}

func NewPreferenceStore(path string) *PreferenceStore {
	return &PreferenceStore{path: path}
}

func (s *PreferenceStore) Get() (Preferences, error) {
	if s == nil {
		return Preferences{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

// ClaimNotification atomically records the version before a notification is shown.
func (s *PreferenceStore) ClaimNotification(version string) (bool, error) {
	if s == nil || strings.TrimSpace(version) == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.readLocked()
	if err != nil {
		return false, err
	}
	if current.NotifiedVersion == version || current.SkippedVersion == version {
		return false, nil
	}
	current.NotifiedVersion = version
	if current.SkippedVersion != "" && current.SkippedVersion != version {
		current.SkippedVersion = ""
	}
	return true, s.writeLocked(current)
}

func (s *PreferenceStore) Skip(version string) error {
	version = strings.TrimSpace(version)
	if s == nil {
		return fmt.Errorf("update preferences are not configured")
	}
	if version == "" {
		return fmt.Errorf("missing version")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.readLocked()
	if err != nil {
		return err
	}
	current.SkippedVersion = version
	return s.writeLocked(current)
}

func (s *PreferenceStore) readLocked() (Preferences, error) {
	if s.path == "" {
		return Preferences{}, fmt.Errorf("empty update preferences path")
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Preferences{}, nil
	}
	if err != nil {
		return Preferences{}, err
	}
	var current Preferences
	if err := json.Unmarshal(data, &current); err != nil {
		backup, backupErr := recovery.BackupCorrupt(s.path)
		if backupErr != nil {
			return Preferences{}, fmt.Errorf("read update preferences: %v; backup corrupt file: %w", err, backupErr)
		}
		log.Printf("recovered update preferences %s after %v; backup=%s", s.path, err, backup)
		return Preferences{}, nil
	}
	return current, nil
}

func (s *PreferenceStore) writeLocked(current Preferences) error {
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		if runtime.GOOS == "windows" {
			if removeErr := os.Remove(s.path); removeErr != nil && !os.IsNotExist(removeErr) {
				return err
			}
			return os.Rename(tmpName, s.path)
		}
		return err
	}
	return nil
}
