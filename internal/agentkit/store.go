package agentkit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"grok_switch/internal/llm"
)

// Store 管理会话目录：~/.grok_switch/agent2/sessions/<id>/
//
//	meta.json       轻量元数据（列表 API 直接读，不解析 transcript）
//	transcript.jsonl 事件追加记录（回放/恢复）
type Store struct {
	root string
	mu   sync.Mutex
}

// SessionMeta 是 meta.json 的内容与列表返回结构（字段与
// agentbridge.SessionSummary 对齐，server 层直接映射）。
type SessionMeta struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Cwd          string    `json:"cwd"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Model        string    `json:"model,omitempty"`
	Engine       string    `json:"engine"` // 固定 "native"
	MessageCount int       `json:"message_count"`
	UserTurns    int       `json:"user_turns"`
}

// NewStore 打开会话存储根目录。
func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("agentkit: 创建会话目录失败: %w", err)
	}
	return &Store{root: root}, nil
}

func (s *Store) sessionDir(id string) string {
	return filepath.Join(s.root, sanitizeID(id))
}

// sanitizeID 防路径穿越：只允许字母数字与连字符下划线。
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "unnamed"
	}
	return out
}

// Create 新建会话并写入初始 meta。
func (s *Store) Create(meta SessionMeta) (SessionMeta, error) {
	if meta.ID == "" {
		meta.ID = newSessionID()
	}
	meta.Engine = "native"
	meta.CreatedAt = time.Now()
	meta.UpdatedAt = meta.CreatedAt
	dir := s.sessionDir(meta.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return SessionMeta{}, err
	}
	if err := s.writeMeta(meta); err != nil {
		return SessionMeta{}, err
	}
	// 空 transcript 占位。
	f, err := os.OpenFile(s.transcriptPath(meta.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return SessionMeta{}, err
	}
	f.Close()
	return meta, nil
}

// Exists 判断会话是否存在。
func (s *Store) Exists(id string) bool {
	_, err := os.Stat(s.metaPath(id))
	return err == nil
}

// GetMeta 读取单个会话 meta。
func (s *Store) GetMeta(id string) (SessionMeta, error) {
	data, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		return SessionMeta{}, err
	}
	var meta SessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return SessionMeta{}, fmt.Errorf("agentkit: meta 解析失败: %w", err)
	}
	return meta, nil
}

// List 返回全部会话（UpdatedAt 倒序）。limit<=0 表示全部。
func (s *Store) List(limit int) ([]SessionMeta, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var out []SessionMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := s.GetMeta(e.Name())
		if err != nil {
			continue // 损坏条目跳过（corrupt 语义与 profiles 一致：不静默删）
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Touch 更新 meta 的可变字段并落盘。
func (s *Store) Touch(id string, fn func(*SessionMeta)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := s.GetMeta(id)
	if err != nil {
		return err
	}
	fn(&meta)
	meta.UpdatedAt = time.Now()
	return s.writeMeta(meta)
}

// Rename 设置标题。
func (s *Store) Rename(id, title string) error {
	return s.Touch(id, func(m *SessionMeta) { m.Title = title })
}

// Delete 删除整个会话目录。
func (s *Store) Delete(id string) error {
	return os.RemoveAll(s.sessionDir(id))
}

// AppendRecord 追加一条 transcript 记录（同步 flush，进程崩溃最多丢一行）。
// 同时递增 meta.MessageCount 并更新 UpdatedAt。
func (s *Store) AppendRecord(id string, rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	f, err := os.OpenFile(s.transcriptPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	// Seq 取当前行数（简化：读现有记录数由调用方维护；这里用文件行数）。
	rec.Seq = int64(countLines(s.transcriptPath(id))) + 1
	b, err := json.Marshal(rec)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	meta, err := s.GetMeta(id)
	if err != nil {
		return err
	}
	meta.MessageCount++
	meta.UpdatedAt = time.Now()
	return s.writeMeta(meta)
}

// LoadRecords 读回全部记录（恢复/回放）。
func (s *Store) LoadRecords(id string) ([]Record, error) {
	f, err := os.Open(s.transcriptPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Record
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // 损坏行跳过
		}
		out = append(out, rec)
	}
	return out, scanner.Err()
}

// RecordsToMessages 把记录投影成回放消息（server 层组装 SessionHistory 用）。
func RecordsToMessages(records []Record) []Record {
	// 过滤空 assistant（纯 tool_call 载体）→ 保留：UI 需要渲染工具调用。
	return records
}

func (s *Store) metaPath(id string) string {
	return filepath.Join(s.sessionDir(id), "meta.json")
}

func (s *Store) transcriptPath(id string) string {
	return filepath.Join(s.sessionDir(id), "transcript.jsonl")
}

func (s *Store) writeMeta(meta SessionMeta) error {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteJSON(s.metaPath(meta.ID), b)
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func atomicWriteJSON(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".meta-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func newSessionID() string {
	return fmt.Sprintf("nat-%d", time.Now().UnixNano())
}

var _ = llm.Role("")
