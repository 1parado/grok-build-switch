package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type Settings struct {
	Port              int      `json:"port"`
	ActualPort        int      `json:"actual_port"`
	Theme             string   `json:"theme"`
	Autostart         bool     `json:"autostart"`
	SilentAutostart   bool     `json:"silent_autostart"`
	AutoOpenBrowser   bool     `json:"auto_open_browser"`
	ProviderOrder     []string `json:"provider_order"`
	PinnedProviderIDs []string `json:"pinned_provider_ids"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func Default() Settings {
	return Settings{
		Port:            17878,
		ActualPort:      17878,
		Theme:           "light",
		Autostart:       false,
		SilentAutostart: true,
		AutoOpenBrowser: true,
	}
}

func (s *Store) Get() (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *Store) Update(next Settings) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next = normalize(next)
	if err := s.writeLocked(next); err != nil {
		return Settings{}, err
	}
	return next, nil
}

func (s *Store) SetActualPort(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.readLocked()
	if err != nil {
		return err
	}
	current.ActualPort = port
	return s.writeLocked(current)
}

func (s *Store) readLocked() (Settings, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return Settings{}, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		def := Default()
		return def, s.writeLocked(def)
	}
	if err != nil {
		return Settings{}, err
	}
	current := Default()
	if len(data) > 0 {
		if err := json.Unmarshal(data, &current); err != nil {
			return Settings{}, fmt.Errorf("read settings: %w", err)
		}
	}
	return normalize(current), nil
}

func (s *Store) writeLocked(current Settings) error {
	data, err := json.MarshalIndent(normalize(current), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*.tmp")
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

func normalize(s Settings) Settings {
	if s.Port == 0 {
		s.Port = 17878
	}
	if s.ActualPort == 0 {
		s.ActualPort = s.Port
	}
	if s.Theme == "" {
		s.Theme = "light"
	}
	s.Theme = "light"
	if s.Autostart {
		s.SilentAutostart = true
	}
	s.ProviderOrder = uniqueStrings(s.ProviderOrder)
	s.PinnedProviderIDs = uniqueStrings(s.PinnedProviderIDs)
	return s
}

func uniqueStrings(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
