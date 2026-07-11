package switcher

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	grokconfig "grok_switch/internal/config"
	"grok_switch/internal/profiles"
)

type Switcher struct {
	ConfigPath string
	BackupsDir string
	Profiles   *profiles.Store
	mu         sync.Mutex
}

type Backup struct {
	File      string    `json:"file"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
	Size      int64     `json:"size"`
}

func (s *Switcher) Activate(id string) (profiles.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	profile, err := s.Profiles.Get(id)
	if err != nil {
		return profiles.Profile{}, err
	}
	if _, err := s.Backup(); err != nil {
		return profiles.Profile{}, err
	}
	if err := grokconfig.ApplyProfileToFile(s.ConfigPath, profile); err != nil {
		return profiles.Profile{}, err
	}
	if err := s.Profiles.SetActive(id); err != nil {
		return profiles.Profile{}, err
	}
	profile.IsActive = true
	return profile, nil
}

func (s *Switcher) ImportCurrent(name string, active bool) (profiles.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	profile, err := grokconfig.ImportProfile(s.ConfigPath, name)
	if err != nil {
		return profiles.Profile{}, err
	}
	profile.IsActive = active
	created, err := s.Profiles.Create(profile)
	if err != nil {
		return profiles.Profile{}, err
	}
	if active {
		if err := s.Profiles.SetActive(created.ID); err != nil {
			return profiles.Profile{}, err
		}
		created.IsActive = true
	}
	return created, nil
}

func (s *Switcher) EnsureDefaultProfile() error {
	profilesList, err := s.Profiles.List()
	if err != nil {
		return err
	}
	if len(profilesList) > 0 {
		return nil
	}
	if _, err := os.Stat(s.ConfigPath); err != nil {
		return err
	}
	_, err = s.ImportCurrent("Default", true)
	return err
}

func (s *Switcher) Backup() (Backup, error) {
	if err := os.MkdirAll(s.BackupsDir, 0o755); err != nil {
		return Backup{}, err
	}
	data, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return Backup{}, err
	}
	now := time.Now()
	file := fmt.Sprintf("config-%s.toml", now.Format("20060102-150405"))
	path := filepath.Join(s.BackupsDir, file)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return Backup{}, err
	}
	if err := s.PruneBackups(10); err != nil {
		return Backup{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Backup{}, err
	}
	return Backup{File: file, Path: path, CreatedAt: info.ModTime(), Size: info.Size()}, nil
}

func (s *Switcher) ListBackups() ([]Backup, error) {
	entries, err := os.ReadDir(s.BackupsDir)
	if os.IsNotExist(err) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	backups := make([]Backup, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".toml") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		backups = append(backups, Backup{
			File:      entry.Name(),
			Path:      filepath.Join(s.BackupsDir, entry.Name()),
			CreatedAt: info.ModTime(),
			Size:      info.Size(),
		})
	}
	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})
	return backups, nil
}

func (s *Switcher) RestoreBackup(file string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if filepath.Base(file) != file || !strings.HasSuffix(strings.ToLower(file), ".toml") {
		return fmt.Errorf("invalid backup file")
	}
	src := filepath.Join(s.BackupsDir, file)
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if _, err := s.Backup(); err != nil {
		return err
	}
	return atomicWrite(s.ConfigPath, data)
}

func (s *Switcher) PruneBackups(keep int) error {
	backups, err := s.ListBackups()
	if err != nil {
		return err
	}
	if keep <= 0 {
		keep = 0
	}
	if len(backups) <= keep {
		return nil
	}
	for _, backup := range backups[keep:] {
		if err := os.Remove(backup.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Switcher) ActiveStatus() (profiles.Profile, bool, error) {
	profilesList, err := s.Profiles.List()
	if err != nil {
		return profiles.Profile{}, false, err
	}
	for _, profile := range profilesList {
		if profile.IsActive {
			matches, err := grokconfig.CurrentMatches(s.ConfigPath, profile)
			if err != nil {
				return profile, false, err
			}
			return profile, matches, nil
		}
	}
	return profiles.Profile{}, false, nil
}

func (s *Switcher) ReadConfig() ([]byte, error) {
	data, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Switcher) WriteConfig(content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.ConfigPath); err == nil {
		if _, err := s.Backup(); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicWrite(s.ConfigPath, content)
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS == "windows" {
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				return err
			}
			return os.Rename(tmpName, path)
		}
		return err
	}
	return nil
}
