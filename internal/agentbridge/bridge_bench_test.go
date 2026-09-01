package agentbridge

import (
	"testing"
	"time"
)

// BenchmarkStatusCached 衡量 /api/agent/status 轮询热路径：优化前每次
// Status() 都 exec.LookPath + os.Stat（≈56µs、16KB、169 allocs，见
// BenchmarkStatusCold）；优化后 30 秒 TTL 内走内存缓存。
func BenchmarkStatusCached(b *testing.B) {
	bridge := New("", "")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = bridge.Status()
	}
}

// BenchmarkStatusCold 强制每次都走真实探测（代表优化前的行为）。
func BenchmarkStatusCold(b *testing.B) {
	bridge := New("", "")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		bridge.mu.Lock()
		bridge.availChecked = time.Time{}
		bridge.mu.Unlock()
		b.StartTimer()
		_ = bridge.Status()
	}
}
