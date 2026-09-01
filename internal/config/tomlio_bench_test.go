package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nowForBench() time.Time { return time.Now() }

// benchmarkConfigDoc 衡量 readDoc 热路径（/api/status 轮询 → CurrentMatches
// → ImportProfile → readDoc）。优化前每次调用都全量 TOML 解析；优化后
// 命中 mtime+size 缓存时只做一次 Stat。Baseline（缓存禁用路径）用
// BenchmarkConfigDocCold 代表。
func benchmarkConfigDoc(b *testing.B, content string) {
	b.Helper()
	path := filepath.Join(b.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		b.Fatal(err)
	}
	// 预热缓存。
	if _, err := readDoc(path); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := readDoc(path); err != nil {
			b.Fatal(err)
		}
	}
}

func configDocForBench(models int) string {
	var sb strings.Builder
	sb.WriteString("[endpoints]\nmodels_base_url = \"https://api.example.com/v1\"\n\n")
	sb.WriteString("[models]\ndefault = \"grok-4.6\"\nweb_search = \"grok-4.6\"\n\n")
	for i := 0; i < models; i++ {
		sb.WriteString("[model.grok-model-" + itoa(i) + "]\n")
		sb.WriteString("model = \"grok-model-" + itoa(i) + "\"\n")
		sb.WriteString("base_url = \"https://api.example.com/v1\"\n")
		sb.WriteString("api_key = \"sk-benchmark\"\n")
		sb.WriteString("supports_reasoning_effort = true\n\n")
	}
	return sb.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func BenchmarkConfigDocCached20(b *testing.B) { benchmarkConfigDoc(b, configDocForBench(20)) }

// BenchmarkConfigDocCold 每次改 mtime 强制走真实解析（代表优化前行为）。
func BenchmarkConfigDocCold(b *testing.B) {
	path := filepath.Join(b.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(configDocForBench(20)), 0o644); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		invalidateDocCache()
		now := nowForBench()
		if err := os.Chtimes(path, now, now); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := readDoc(path); err != nil {
			b.Fatal(err)
		}
	}
}
