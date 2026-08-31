package agentkit

// Compaction handoff（设计文档 §6.4，语义对齐 Kimi compaction/handoff.ts）：
//
// 触发：上一步真实 usage ≥ 85% 窗口，或 usage + 预留输出 ≥ 窗口（D3）。
// 重写：保留用户原话（字符预算内，最老 2k token 强制保留形成 head/tail +
// 省略标记）+ 一条 COMPACTION_SUMMARY 前缀的摘要消息（明确告知模型这是交接
// 上下文，不是真实用户输入）。assistant/tool 消息全部折叠进摘要。
// PromptOrigin 是正确性根基：只有 OriginUser 是真实用户输入，其余可重建。

import (
	"strings"

	"grok_switch/internal/llm"
)

// CompactionConfig 是压实参数（默认值对齐设计文档）。
type CompactionConfig struct {
	// TriggerRatio 触发阈值（占窗口比例）。
	TriggerRatio float64
	// ReservedContextSize 预留输出预算（token）。
	ReservedContextSize int64
	// KeptUserTokens 保留用户原话的 token 预算。
	KeptUserTokens int64
	// HeadUserTokens head 段预算（最老的用户输入，通常是最初任务陈述）。
	HeadUserTokens int64
	// MaxOverflowAttempts 摘要请求本身溢出时的最大重试。
	MaxOverflowAttempts int
}

// DefaultCompactionConfig 默认参数：85% 触发、50k 保留输出、
// 20k 用户原话预算（head 2k）——与 Kimi DEFAULT_COMPACTION_CONFIG 一致。
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		TriggerRatio:        0.85,
		ReservedContextSize: 50_000,
		KeptUserTokens:      20_000,
		HeadUserTokens:      2_000,
		MaxOverflowAttempts: 3,
	}
}

// COMPACTION_SUMMARY_PREFIX 出现在摘要消息开头，让模型识别这是交接上下文。
const COMPACTION_SUMMARY_PREFIX = "COMPACTION_SUMMARY: 以下是对更早对话的交接摘要，不是用户的真实输入。"

// COMPACTION_ELISION_MARKER 填在 head 与 tail 之间，告知模型中间省略了什么。
const COMPACTION_ELISION_MARKER = "[……中间的用户消息已省略，摘要见上……]"

// ShouldCompact 判断是否需要压实。usedTokens 来自上一步真实 usage
// （D3：不用 tokenizer；无 usage 时调用方传入估算值并把阈值放宽 10%）。
func (c CompactionConfig) ShouldCompact(usedTokens, maxContext int64) bool {
	if maxContext <= 0 || usedTokens <= 0 {
		return false
	}
	if usedTokens >= int64(float64(maxContext)*c.TriggerRatio) {
		return true
	}
	if c.ReservedContextSize > 0 && c.ReservedContextSize < maxContext {
		return usedTokens+c.ReservedContextSize >= maxContext
	}
	return false
}

// Compact 是压实的纯函数核心：输入全部消息（与 origins 平行）与摘要生成器，
// 输出压实后的 (消息, origins) 及统计。original 只读，不修改。
//
// 摘要生成器 summarizer 由宿主提供（调用一次 LLM，输入被折叠的消息文本）；
// 它返回摘要文本。近似 token 用 EstimateTokens（字符/4）。
func Compact(msgs []llm.Message, origins []Origin, summarizer func(dropped []llm.Message) (string, error), cfg CompactionConfig) ([]llm.Message, []Origin, CompactionStats, error) {
	stats := CompactionStats{}
	if len(msgs) != len(origins) {
		return nil, nil, stats, ErrOriginMismatch
	}

	// 1. 挑选保留的用户原话（OriginUser 且有文本）。
	type keptUser struct{ idx int }
	var userMsgs []keptUser
	for i, o := range origins {
		if o == OriginUser && msgs[i].Text() != "" {
			userMsgs = append(userMsgs, keptUser{idx: i})
		}
	}
	stats.UserMessagesTotal = len(userMsgs)

	// 2. 在预算内从最新往回保留（tail），再保证最老 HeadUserTokens（head）。
	budget := cfg.KeptUserTokens
	headBudget := cfg.HeadUserTokens
	if headBudget >= budget {
		headBudget = budget / 4
	}
	keptSet := map[int]bool{}
	used := int64(0)
	for i := len(userMsgs) - 1; i >= 0; i-- {
		m := msgs[userMsgs[i].idx]
		cost := llm.EstimateMessageTokens(m)
		if used+cost > budget {
			continue // 超预算的单条跳过（继续尝试更老的小消息）
		}
		keptSet[userMsgs[i].idx] = true
		used += cost
	}
	// head：从最老开始，在 headBudget 内强制保留（可能与 tail 重叠）。
	headUsed := int64(0)
	headCount := 0
	for _, u := range userMsgs {
		if keptSet[u.idx] {
			headCount++
			continue
		}
		m := msgs[u.idx]
		cost := llm.EstimateMessageTokens(m)
		if headUsed+cost > headBudget {
			break
		}
		keptSet[u.idx] = true
		headUsed += cost
		headCount++
	}
	if headCount > len(userMsgs) {
		headCount = len(userMsgs)
	}
	stats.KeptUserMessages = len(keptSet)
	stats.KeptHeadUserMessages = headCount

	// 3. 收集被折叠的消息（非保留用户消息 + 全部 assistant/tool）。
	var dropped []llm.Message
	var droppedUserCount int
	for i, m := range msgs {
		if origins[i] == OriginUser && keptSet[i] {
			continue
		}
		// 已有的摘要与省略标记不重复折叠（Kimi：markers never stack）。
		if origins[i] == OriginSummary || origins[i] == OriginInjection {
			continue
		}
		dropped = append(dropped, m)
		if origins[i] == OriginUser {
			droppedUserCount++
		}
	}
	stats.CompactedCount = len(dropped)
	stats.DroppedUserMessages = droppedUserCount

	if len(dropped) == 0 {
		// 没有可折叠内容：原样返回。
		return msgs, origins, stats, nil
	}

	// 4. 生成摘要。
	summary, err := summarizer(dropped)
	if err != nil {
		return nil, nil, stats, err
	}

	// 5. 组装新序列：head 用户原话 → 省略标记（若有跳过）→ tail 用户原话 → 摘要。
	var out []llm.Message
	var outOrigins []Origin
	lastKeptIdx := -1
	needElision := false
	for i, m := range msgs {
		if origins[i] != OriginUser || !keptSet[i] {
			continue
		}
		if needElision && lastKeptIdx >= 0 && hasGapBetween(msgs, origins, lastKeptIdx, i) {
			out = append(out, llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: COMPACTION_ELISION_MARKER}}})
			outOrigins = append(outOrigins, OriginInjection)
		}
		out = append(out, m)
		outOrigins = append(outOrigins, OriginUser)
		lastKeptIdx = i
		needElision = true
	}
	// 摘要消息（user 角色承载，但 Origin=summary；compaction 时不再重复折叠）。
	out = append(out, llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: COMPACTION_SUMMARY_PREFIX + "\n\n" + summary}}})
	outOrigins = append(outOrigins, OriginSummary)

	stats.TokensBefore = llm.EstimateHistoryTokens(msgs)
	stats.TokensAfter = llm.EstimateHistoryTokens(out)
	return out, outOrigins, stats, nil
}

// hasGapBetween 判断两个保留用户消息之间是否存在被省略的用户消息。
func hasGapBetween(msgs []llm.Message, origins []Origin, from, to int) bool {
	for i := from + 1; i < to; i++ {
		if origins[i] == OriginUser {
			return true
		}
	}
	return false
}

// CompactionStats 是一次压实的统计（遥测/事件用）。
type CompactionStats struct {
	UserMessagesTotal    int
	KeptUserMessages     int
	KeptHeadUserMessages int
	CompactedCount       int
	DroppedUserMessages  int
	TokensBefore         int64
	TokensAfter          int64
}

// CompactInPlace 对 CtxMemory 应用压实结果（宿主层在 BeforeStep hook 调用）。
func (m *CtxMemory) CompactInPlace(msgs []llm.Message, origins []Origin, stats CompactionStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = msgs
	m.origins = origins
	m.userTurns = 0
	for _, o := range origins {
		if o == OriginUser {
			m.userTurns++
		}
	}
}

// 错误定义。
var ErrOriginMismatch = compactionError("agentkit: 消息与来源数组长度不一致")

type compactionError string

func (e compactionError) Error() string { return string(e) }

// DropOldest 在摘要请求本身溢出时的兜底：从待摘要消息里砍掉最老的 n 条
// （诚实统计 blind spot，Kimi droppedCount 语义）。
func DropOldest(msgs []llm.Message, dropTokens int64) ([]llm.Message, int) {
	used := int64(0)
	i := 0
	for ; i < len(msgs); i++ {
		used += llm.EstimateMessageTokens(msgs[i])
		if used > dropTokens {
			break
		}
	}
	if i >= len(msgs) {
		return nil, len(msgs)
	}
	return msgs[i:], i
}

// CompactInputText 把被折叠的消息压成摘要器的输入文本（截断到预算）。
func CompactInputText(dropped []llm.Message, maxChars int) string {
	var b strings.Builder
	for _, m := range dropped {
		if b.Len() >= maxChars {
			break
		}
		switch m.Role {
		case llm.RoleUser, llm.RoleAssistant:
			if txt := m.Text(); txt != "" {
				b.WriteString(roleLabel(m.Role))
				b.WriteString(": ")
				b.WriteString(truncateRunes(txt, 2000))
				b.WriteString("\n")
			}
			for _, tc := range m.ToolCalls {
				b.WriteString("tool_call: " + tc.Name + "\n")
			}
		case llm.RoleTool:
			b.WriteString("tool_result: ")
			b.WriteString(truncateRunes(m.Text(), 500))
			b.WriteString("\n")
		}
	}
	if b.Len() > maxChars {
		return b.String()[:maxChars] + "\n[输入过长已截断]"
	}
	return b.String()
}

func roleLabel(r llm.Role) string {
	if r == llm.RoleUser {
		return "user"
	}
	return "assistant"
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
