package profiles

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// benchmarkProfiles 建一个含 10 个 profile 的 store，衡量 List 热路径。
// 优化前每次 List 都读盘 + JSON 反序列化 + Normalize；优化后命中
// mtime+size 缓存时只做一次 Stat 和切片拷贝。
func benchmarkProfiles(b *testing.B, n int) {
	b.Helper()
	path := filepath.Join(b.TempDir(), "profiles.json")
	store := NewStore(path)
	for i := 0; i < n; i++ {
		if _, err := store.Create(profileForBench(i)); err != nil {
			b.Fatal(err)
		}
	}
	// 预热缓存，模拟服务运行态。
	if _, err := store.List(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.List(); err != nil {
			b.Fatal(err)
		}
	}
}

func profileForBench(i int) Profile {
	return Profile{
		Name:           "provider",
		UpstreamFormat: "openai_chat",
		BaseURL:        "https://api.example.com/v1",
		APIKey:         "sk-benchmark",
		DefaultModel:   "grok-4.6",
		AvailableModels: []string{
			"grok-4.6", "grok-4.5", "grok-4", "grok-3", "grok-code",
		},
		Models: []ModelDef{
			{Name: "grok-4.6", Model: "grok-4.6"},
			{Name: "grok-4.5", Model: "grok-4.5"},
		},
	}
}

func BenchmarkStoreList10(b *testing.B)  { benchmarkProfiles(b, 10) }
func BenchmarkStoreList100(b *testing.B) { benchmarkProfiles(b, 100) }

// BenchmarkStoreListColdRead 强制缓存失效，衡量磁盘路径（用于对比缓存收益）。
func BenchmarkStoreListColdRead(b *testing.B) {
	path := filepath.Join(b.TempDir(), "profiles.json")
	store := NewStore(path)
	for i := 0; i < 100; i++ {
		if _, err := store.Create(profileForBench(i)); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// 触碰文件 mtime 使缓存失效（不改变内容）。
		now := time.Now()
		if err := os.Chtimes(path, now, now.Add(time.Millisecond)); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := store.List(); err != nil {
			b.Fatal(err)
		}
	}
}
