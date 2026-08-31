package agentloop

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// JSONLRecorder 把事件流按行追加写入 JSONL 文件（transcript 持久化的雏形）。
// 每次 Dispatch 同步 flush——引擎事件频率低（步/工具粒度），吞吐不是瓶颈，
// 换取进程崩溃时最多丢一行。
type JSONLRecorder struct {
	mu   sync.Mutex
	file *os.File
	w    *bufio.Writer
}

// NewJSONLRecorder 打开（或创建）录制文件。目录不存在会自动创建。
func NewJSONLRecorder(path string) (*JSONLRecorder, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("agentloop: 创建录制目录失败: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("agentloop: 打开录制文件失败: %w", err)
	}
	return &JSONLRecorder{file: f, w: bufio.NewWriter(f)}, nil
}

func (r *JSONLRecorder) Dispatch(ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == nil {
		return
	}
	r.w.Write(b)
	r.w.WriteByte('\n')
	r.w.Flush()
}

// Close 落盘并关闭文件。
func (r *JSONLRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w == nil {
		return nil
	}
	flushErr := r.w.Flush()
	file := r.file
	r.w = nil
	r.file = nil
	if flushErr != nil {
		file.Close()
		return flushErr
	}
	return file.Close()
}

// MemoryRecorder 把事件收进内存切片（测试用）。
type MemoryRecorder struct {
	mu     sync.Mutex
	Events []Event
}

func (m *MemoryRecorder) Dispatch(ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Events = append(m.Events, ev)
}

// Snapshot 返回事件副本。
func (m *MemoryRecorder) Snapshot() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.Events))
	copy(out, m.Events)
	return out
}

// FilterTypes 按类型筛选快照。
func (m *MemoryRecorder) FilterTypes(types ...EventType) []Event {
	want := map[EventType]bool{}
	for _, t := range types {
		want[t] = true
	}
	var out []Event
	for _, ev := range m.Snapshot() {
		if want[ev.Type] {
			out = append(out, ev)
		}
	}
	return out
}
