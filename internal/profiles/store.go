package profiles

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) List() ([]Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *Store) Get(id string) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles, err := s.readLocked()
	if err != nil {
		return Profile{}, err
	}
	for _, profile := range profiles {
		if profile.ID == id {
			return profile, nil
		}
	}
	return Profile{}, os.ErrNotExist
}

func (s *Store) Create(profile Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles, err := s.readLocked()
	if err != nil {
		return Profile{}, err
	}
	now := time.Now()
	if profile.ID == "" {
		profile.ID = newID()
	}
	if profile.CreatedAt.IsZero() {
		profile.CreatedAt = now
	}
	profile = Normalize(profile)
	profile.UpdatedAt = now
	profiles = append(profiles, profile)
	if err := s.writeLocked(profiles); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func (s *Store) Update(id string, next Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles, err := s.readLocked()
	if err != nil {
		return Profile{}, err
	}
	for i := range profiles {
		if profiles[i].ID == id {
			next.ID = id
			next.CreatedAt = profiles[i].CreatedAt
			next.UpdatedAt = time.Now()
			next.IsActive = profiles[i].IsActive
			next = Normalize(next)
			profiles[i] = next
			if err := s.writeLocked(profiles); err != nil {
				return Profile{}, err
			}
			return next, nil
		}
	}
	return Profile{}, os.ErrNotExist
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles, err := s.readLocked()
	if err != nil {
		return err
	}
	next := profiles[:0]
	found := false
	for _, profile := range profiles {
		if profile.ID == id {
			found = true
			continue
		}
		next = append(next, profile)
	}
	if !found {
		return os.ErrNotExist
	}
	return s.writeLocked(next)
}

func (s *Store) SetActive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles, err := s.readLocked()
	if err != nil {
		return err
	}
	found := false
	for i := range profiles {
		profiles[i].IsActive = profiles[i].ID == id
		if profiles[i].IsActive {
			profiles[i].UpdatedAt = time.Now()
			found = true
		}
	}
	if !found {
		return os.ErrNotExist
	}
	return s.writeLocked(profiles)
}

func (s *Store) ClearActive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles, err := s.readLocked()
	if err != nil {
		return err
	}
	changed := false
	for i := range profiles {
		if profiles[i].IsActive {
			profiles[i].IsActive = false
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.writeLocked(profiles)
}

func (s *Store) EnsureDir() error {
	return os.MkdirAll(filepath.Dir(s.path), 0o755)
}

func (s *Store) readLocked() ([]Profile, error) {
	if err := s.EnsureDir(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []Profile{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return []Profile{}, nil
	}
	var profiles []Profile
	if err := json.Unmarshal(data, &profiles); err != nil {
		return nil, fmt.Errorf("read profiles: %w", err)
	}
	for i := range profiles {
		profiles[i] = Normalize(profiles[i])
	}
	return profiles, nil
}

func (s *Store) writeLocked(profiles []Profile) error {
	data, err := json.MarshalIndent(profiles, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.path, append(data, '\n'))
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

func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}
