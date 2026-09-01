package agentkit

import (
	"fmt"
	"testing"

	"grok_switch/internal/llm"
)

// benchmarkAppendRecord 衡量 turn 内逐事件落盘的热路径。优化前每条记录
// 都要全量读一遍 transcript.jsonl 数行数（文件线性增长 → 每条追加 O(n)，
// 长会话整体 O(n²)）；优化后用内存计数器，每条追加 O(1)。
func benchmarkAppendRecord(b *testing.B, prefill int) {
	b.Helper()
	store, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	meta, err := store.Create(SessionMeta{Cwd: "/tmp"})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < prefill; i++ {
		if err := store.AppendRecord(meta.ID, Record{Origin: OriginTool, Role: llm.RoleTool, Text: fmt.Sprintf("tool output %d", i)}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.AppendRecord(meta.ID, Record{Origin: OriginTool, Role: llm.RoleTool, Text: "benchmark record"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendRecordFresh(b *testing.B) { benchmarkAppendRecord(b, 0) }
func BenchmarkAppendRecord1k(b *testing.B)    { benchmarkAppendRecord(b, 1000) }
func BenchmarkAppendRecord10k(b *testing.B)   { benchmarkAppendRecord(b, 10000) }
