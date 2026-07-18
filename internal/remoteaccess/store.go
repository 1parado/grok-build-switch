package remoteaccess

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const pairingLifetime = 10 * time.Minute

type Snapshot struct {
	SessionToken  string
	PairingCode   string
	PairingExpiry time.Time
}

type persistedState struct {
	SessionToken  string    `json:"session_token"`
	PairingCode   string    `json:"pairing_code,omitempty"`
	PairingExpiry time.Time `json:"pairing_expiry,omitempty"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store {
	return &Store{path: path}
}

func (s *Store) Get() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot(state), nil
}

func (s *Store) NewPairing() (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return Snapshot{}, err
	}
	state.PairingCode, err = randomPairingCode()
	if err != nil {
		return Snapshot{}, err
	}
	state.PairingExpiry = time.Now().Add(pairingLifetime)
	if err := s.saveLocked(state); err != nil {
		return Snapshot{}, err
	}
	return snapshot(state), nil
}

func (s *Store) ConsumePairing(code string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return "", false, err
	}
	valid := code != "" && state.PairingCode != "" &&
		subtle.ConstantTimeCompare([]byte(code), []byte(state.PairingCode)) == 1 &&
		time.Now().Before(state.PairingExpiry)
	if !valid {
		return "", false, nil
	}
	state.PairingCode = ""
	state.PairingExpiry = time.Time{}
	if err := s.saveLocked(state); err != nil {
		return "", false, err
	}
	return state.SessionToken, true, nil
}

func (s *Store) Authorized(token string) (bool, error) {
	snapshot, err := s.Get()
	if err != nil {
		return false, err
	}
	if token == "" || snapshot.SessionToken == "" {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(snapshot.SessionToken)) == 1, nil
}

func (s *Store) ResetSessions() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	state.SessionToken, err = randomToken("sess-")
	if err != nil {
		return err
	}
	state.PairingCode = ""
	state.PairingExpiry = time.Time{}
	return s.saveLocked(state)
}

func (s *Store) loadLocked() (persistedState, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		state := persistedState{}
		state.SessionToken, err = randomToken("sess-")
		if err != nil {
			return persistedState{}, err
		}
		if err := s.saveLocked(state); err != nil {
			return persistedState{}, err
		}
		return state, nil
	}
	if err != nil {
		return persistedState{}, err
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return persistedState{}, fmt.Errorf("读取局域网访问凭据失败: %w", err)
	}
	if state.SessionToken == "" {
		state.SessionToken, err = randomToken("sess-")
		if err != nil {
			return persistedState{}, err
		}
		if err := s.saveLocked(state); err != nil {
			return persistedState{}, err
		}
	}
	return state, nil
}

func (s *Store) saveLocked(state persistedState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		if runtime.GOOS == "windows" {
			if removeErr := os.Remove(s.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return err
			}
			return os.Rename(tmpName, s.path)
		}
		return err
	}
	return nil
}

func snapshot(state persistedState) Snapshot {
	return Snapshot{
		SessionToken:  state.SessionToken,
		PairingCode:   state.PairingCode,
		PairingExpiry: state.PairingExpiry,
	}
}

func randomToken(prefix string) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}

func randomPairingCode() (string, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%08d", value.Int64()), nil
}
