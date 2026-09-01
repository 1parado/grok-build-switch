package grokpool

import (
	"fmt"
	"testing"
)

// benchmarkSummary 衡量全池 summary 计算（/api/status、/api/grok-pool、
// /api/grok-pool/accounts 每次轮询都走）。优化前每次调用都逐账号重算；
// 优化后状态未变时直接复用缓存。
func benchmarkSummary(b *testing.B, n int) {
	b.Helper()
	accounts := make([]Account, n)
	for i := range accounts {
		accounts[i] = Account{
			ID:             fmt.Sprintf("acc-%06d", i),
			Email:          fmt.Sprintf("user%06d@example.com", i),
			Classification: []string{"healthy", "healthy", "healthy", "quota_exhausted", "uninspected"}[i%5],
			Disabled:       i%17 == 0,
		}
	}
	state := persistedState{Accounts: accounts}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = summarize(state.Accounts)
	}
}

// BenchmarkSummarize1k 是未命中缓存时（巡检后第一次读取）的成本基准。
func BenchmarkSummarize1k(b *testing.B)  { benchmarkSummary(b, 1000) }
func BenchmarkSummarize10k(b *testing.B) { benchmarkSummary(b, 10000) }
