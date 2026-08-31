package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"grok_switch/internal/recovery"
)

const (
	MinPort = 1024
	MaxPort = 65535
)

type Settings struct {
	Port              int      `json:"port"`
	ActualPort        int      `json:"actual_port"`
	Theme             string   `json:"theme"`
	Autostart         bool     `json:"autostart"`
	SilentAutostart   bool     `json:"silent_autostart"`
	AutoOpenBrowser   bool     `json:"auto_open_browser"`
	LANAccessEnabled  bool     `json:"lan_access_enabled"`
	AgentDefaultCwd   string   `json:"agent_default_cwd,omitempty"`
	ProviderOrder     []string `json:"provider_order"`
	PinnedProviderIDs []string `json:"pinned_provider_ids"`
	// ImageGenEnabled 是生图能力的全局开关：开启时向 Grok CLI 注册生图 MCP
	// 服务器并接管 image_gen 请求（走账号池）；关闭时移除注册、不做接管，
	// 模型回到无生图能力的原始状态。对所有供应商生效。
	ImageGenEnabled bool `json:"image_gen_enabled"`
	// AgentEngine 选择聊天工作台的 Agent 引擎（设计文档 D6）：
	//   "acp"    — spawn Grok CLI 走 ACP 协议（旧行为，默认）
	//   "native" — 进程内自研引擎（internal/agentloop），无需 grok CLI
	// 空值按 "acp" 处理；启动失败自动降级 acp 并在 UI 提示。
	AgentEngine string `json:"agent_engine,omitempty"`
}

// 引擎选择器的合法取值。
const (
	AgentEngineACP    = "acp"
	AgentEngineNative = "native"
)

// NormalizedAgentEngine 返回归一化的引擎名（空/未知 → acp）。
func (s Settings) NormalizedAgentEngine() string {
	if s.AgentEngine == AgentEngineNative {
		return AgentEngineNative
	}
	return AgentEngineACP
}

type Store struct {
	path string
	mu   sync.Mutex

	// 热路径缓存：Get() 位于每个本地代理请求的路径上，逐次读盘 + JSON 解析
	// 是纯开销。settings.json 只由本进程写入（单实例），用 mtime+size 判断
	// 是否需要重新读取即可。
	cacheValid bool
	cacheValue Settings
	cacheMod   time.Time
	cacheSize  int64
}

type ValidationError struct {
	Field string
	Err   error
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Err.Error()
}

func (e *ValidationError) Unwrap() error { return e.Err }

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
		ImageGenEnabled: true,
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
	if err := Validate(next); err != nil {
		return Settings{}, err
	}
	if err := s.writeLocked(next); err != nil {
		return Settings{}, err
	}
	return next, nil
}

func (s *Store) SetActualPort(port int) error {
	if err := ValidatePort(port); err != nil {
		return err
	}
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
	var modTime time.Time
	var size int64
	cacheHit := false
	if info, err := os.Stat(s.path); err == nil {
		modTime, size = info.ModTime(), info.Size()
		cacheHit = s.cacheValid && modTime.Equal(s.cacheMod) && size == s.cacheSize
	}
	if cacheHit {
		return s.cacheValue, nil
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
			return s.recoverLocked(fmt.Errorf("read settings: %w", err), Default())
		}
	}
	current = normalize(current)
	if err := Validate(current); err != nil {
		repaired := current
		if ValidatePort(repaired.Port) != nil {
			repaired.Port = Default().Port
		}
		if ValidatePort(repaired.ActualPort) != nil {
			repaired.ActualPort = repaired.Port
		}
		return s.recoverLocked(fmt.Errorf("invalid settings: %w", err), repaired)
	}
	s.cacheValid = true
	s.cacheValue = current
	s.cacheMod = modTime
	s.cacheSize = size
	return current, nil
}

func (s *Store) recoverLocked(cause error, recovered Settings) (Settings, error) {
	backup, err := recovery.BackupCorrupt(s.path)
	if err != nil {
		return Settings{}, fmt.Errorf("%v; backup corrupt settings: %w", cause, err)
	}
	log.Printf("recovered settings file %s after %v; backup=%s", s.path, cause, backup)
	recovered = normalize(recovered)
	if err := s.writeLocked(recovered); err != nil {
		return Settings{}, fmt.Errorf("restore default settings after %v: %w", cause, err)
	}
	return recovered, nil
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
	// 写入成功后同步缓存；Stat 失败则失效缓存，下次读取时重建。
	s.cacheValue = normalize(current)
	if info, err := os.Stat(s.path); err == nil {
		s.cacheValid = true
		s.cacheMod = info.ModTime()
		s.cacheSize = info.Size()
	} else {
		s.cacheValid = false
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
	// theme 三态：light / dark / auto（跟随系统）；未知值回退 light。
	switch s.Theme {
	case "light", "dark", "auto":
	default:
		s.Theme = "light"
	}
	if s.Autostart {
		s.SilentAutostart = true
	}
	s.ProviderOrder = uniqueStrings(s.ProviderOrder)
	s.PinnedProviderIDs = uniqueStrings(s.PinnedProviderIDs)
	s.AgentDefaultCwd = filepath.Clean(s.AgentDefaultCwd)
	if s.AgentDefaultCwd == "." {
		s.AgentDefaultCwd = ""
	}
	s.AgentEngine = s.NormalizedAgentEngine()
	return s
}

func Validate(s Settings) error {
	if err := ValidatePort(s.Port); err != nil {
		return &ValidationError{Field: "port", Err: err}
	}
	if err := ValidatePort(s.ActualPort); err != nil {
		return &ValidationError{Field: "actual_port", Err: err}
	}
	return nil
}

func IsValidationError(err error) bool {
	var validationErr *ValidationError
	return errors.As(err, &validationErr)
}

func ValidatePort(port int) error {
	if port < MinPort || port > MaxPort {
		return fmt.Errorf("端口必须在 %d–%d 之间", MinPort, MaxPort)
	}
	return nil
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
