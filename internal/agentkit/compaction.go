package agentkit

// Compaction handoff（设计文档 §6.4，语义对齐 Kimi compaction/handoff.ts）：
//
// 触发：上一步真实 usage ≥ 85% 窗口，或 usage + 预留输出 ≥ 窗口（D3）。
// 重写：保留用户原话（字符预算内，最老 2k token 强制保留形成 head/tail +
// 省略标记）+ 一条 COMPACTION_SUMMARY 前缀的摘要消息（明确告知模型这是交接
// 上下文，不是真实用户输入）。assistant/tool 消息全部折叠进摘要。
// PromptOrigin 是正确性根基：只有 OriginUser 是真实用户输入，其余可重建。

import (
	"encoding/json"
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
	// KeepRecentTokens 尾部完整保留预算（token，含 assistant/tool 不折叠）。
	// 对齐 pi keepRecentTokens：近期上下文原样保留，只摘要更老部分。
	// 0 表示禁用尾部保留（测试/兼容旧行为）。
	KeepRecentTokens int64
	// MaxOverflowAttempts 摘要请求本身溢出时的最大重试。
	MaxOverflowAttempts int
}

// DefaultCompactionConfig 默认参数：85% 触发、50k 保留输出、
// 20k 用户原话预算（head 2k）+ 20k 尾部完整保留——与 Kimi DEFAULT_COMPACTION_CONFIG 一致。
func DefaultCompactionConfig() CompactionConfig {
	return CompactionConfig{
		TriggerRatio:        0.85,
		ReservedContextSize: 50_000,
		KeptUserTokens:      20_000,
		HeadUserTokens:      2_000,
		KeepRecentTokens:    20_000,
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
//
// 语义（对齐 pi/Kimi）：
//   - KeepRecentTokens>0 时尾部按 token 完整保留（含 assistant/tool），只摘要更老部分，
//     切点永不在 tool_result 处切断（保 toolCall-result 相邻）。
//   - 头部在 KeptUserTokens/HeadUserTokens 内保留用户原话，形成 head/tail + 省略标记。
//   - 摘要尾附 <read-files>/<modified-files> 文件操作清单，避免路径丢失。
func Compact(msgs []llm.Message, origins []Origin, summarizer func(dropped []llm.Message) (string, error), cfg CompactionConfig) ([]llm.Message, []Origin, CompactionStats, error) {
	stats := CompactionStats{}
	if len(msgs) != len(origins) {
		return nil, nil, stats, ErrOriginMismatch
	}

	// 0. 尾部切点：从尾累加到 KeepRecentTokens，永不在 tool_result 处切。
	cutIdx := 0
	if cfg.KeepRecentTokens > 0 && len(msgs) > 0 {
		cutIdx = findTailCut(msgs, cfg.KeepRecentTokens)
	}
	var tailMsgs []llm.Message
	var tailOrigins []Origin
	var headMsgs []llm.Message
	var headOrigins []Origin
	if cutIdx > 0 && cutIdx < len(msgs) {
		headMsgs, headOrigins = msgs[:cutIdx], origins[:cutIdx]
		tailMsgs, tailOrigins = msgs[cutIdx:], origins[cutIdx:]
	} else {
		// 无尾部保留（测试零值/小历史）：全量走旧路径。
		headMsgs, headOrigins = msgs, origins
	}

	// 1. 在头部挑选保留的用户原话（OriginUser 且有文本）。
	type keptUser struct{ idx int }
	var userMsgs []keptUser
	for i, o := range headOrigins {
		if o == OriginUser && headMsgs[i].Text() != "" {
			userMsgs = append(userMsgs, keptUser{idx: i})
		}
	}
	// UserMessagesTotal 统计全量（含尾部），与旧语义一致。
	totalUsers := 0
	for _, o := range origins {
		if o == OriginUser {
			totalUsers++
		}
	}
	stats.UserMessagesTotal = totalUsers

	// 2. 在预算内从最新往回保留（tail），再保证最老 HeadUserTokens（head）。
	budget := cfg.KeptUserTokens
	headBudget := cfg.HeadUserTokens
	if headBudget >= budget && budget > 0 {
		headBudget = budget / 4
	}
	keptSet := map[int]bool{}
	used := int64(0)
	for i := len(userMsgs) - 1; i >= 0; i-- {
		m := headMsgs[userMsgs[i].idx]
		cost := llm.EstimateMessageTokens(m)
		if budget > 0 && used+cost > budget {
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
		m := headMsgs[u.idx]
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

	// 3. 收集被折叠的头部消息（非保留用户消息 + 全部 assistant/tool）。
	var dropped []llm.Message
	var droppedUserCount int
	for i, m := range headMsgs {
		if headOrigins[i] == OriginUser && keptSet[i] {
			continue
		}
		// 已有的摘要与省略标记不重复折叠（Kimi：markers never stack）。
		if headOrigins[i] == OriginSummary || headOrigins[i] == OriginInjection {
			continue
		}
		dropped = append(dropped, m)
		if headOrigins[i] == OriginUser {
			droppedUserCount++
		}
	}
	stats.CompactedCount = len(dropped)
	stats.DroppedUserMessages = droppedUserCount
	stats.RetainedTailCount = len(tailMsgs)
	stats.RetainedTailTokens = llm.EstimateHistoryTokens(tailMsgs)

	if len(dropped) == 0 {
		// 没有可折叠内容：原样返回（尾部保留无意义，直接返回原序列）。
		return msgs, origins, stats, nil
	}

	// 4. 生成摘要，尾附文件操作清单。
	summary, err := summarizer(dropped)
	if err != nil {
		return nil, nil, stats, err
	}
	readFiles, modifiedFiles := extractFileOps(dropped)
	stats.ReadFiles = readFiles
	stats.ModifiedFiles = modifiedFiles
	if fileOps := formatFileOps(readFiles, modifiedFiles); fileOps != "" {
		summary = summary + "\n\n" + fileOps
	}

	// 5. 组装新序列：head 用户原话 → 省略标记 → 摘要 → 完整尾部。
	var out []llm.Message
	var outOrigins []Origin
	lastKeptIdx := -1
	needElision := false
	for i, m := range headMsgs {
		if headOrigins[i] != OriginUser || !keptSet[i] {
			continue
		}
		if needElision && lastKeptIdx >= 0 && hasGapBetween(headMsgs, headOrigins, lastKeptIdx, i) {
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
	// 尾部原样追加（保持 toolCall-result 相邻与近期完整性）。
	out = append(out, tailMsgs...)
	outOrigins = append(outOrigins, tailOrigins...)

	stats.TokensBefore = llm.EstimateHistoryTokens(msgs)
	stats.TokensAfter = llm.EstimateHistoryTokens(out)
	return out, outOrigins, stats, nil
}

// findTailCut 从尾累加 token 到预算，返回头部/尾部分界下标。
// 不变量：切点永不在 tool_result 处（若首条保留是 tool 结果则前移含其 assistant 调用）。
func findTailCut(msgs []llm.Message, budget int64) int {
	if budget <= 0 || len(msgs) == 0 {
		return 0
	}
	var acc int64
	cut := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		acc += llm.EstimateMessageTokens(msgs[i])
		if acc >= budget {
			cut = i
			break
		}
		// 全量不足预算：无需切分。
		if i == 0 {
			return 0
		}
	}
	// 前移吞掉孤儿 tool_result 的配对 assistant（含多结果批次）。
	for cut > 0 && msgs[cut].Role == llm.RoleTool {
		cut--
	}
	return cut
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
	RetainedTailCount    int
	RetainedTailTokens   int64
	ReadFiles            []string
	ModifiedFiles        []string
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
				b.WriteString("tool_call: " + tc.Name + " " + truncateRunes(string(tc.Arguments), 500) + "\n")
			}
		case llm.RoleTool:
			b.WriteString("tool_result: ")
			b.WriteString(truncateRunes(m.Text(), 2000))
			b.WriteString("\n")
		}
	}
	if b.Len() > maxChars {
		return truncateRunes(b.String(), maxChars) + "\n[输入过长已截断]"
	}
	return b.String()
}

// --- 文件操作追踪（对齐 pi formatFileOperations） ---

// extractFileOps 从被折叠消息的工具调用中提取读写文件清单。
func extractFileOps(dropped []llm.Message) (readFiles, modifiedFiles []string) {
	seenRead := map[string]bool{}
	seenMod := map[string]bool{}
	add := func(dst *[]string, seen map[string]bool, v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		*dst = append(*dst, v)
	}
	for _, m := range dropped {
		for _, tc := range m.ToolCalls {
			path := toolCallPath(tc.Name, tc.Arguments)
			if path == "" {
				continue
			}
			switch tc.Name {
			case "write", "edit":
				add(&modifiedFiles, seenMod, path)
			default:
				// read/glob/grep 等只读工具；未知工具按只读归类。
				if !seenMod[path] {
					add(&readFiles, seenRead, path)
				}
			}
		}
	}
	// 读过又改过的文件只算 modified。
	filtered := readFiles[:0]
	for _, f := range readFiles {
		if !seenMod[f] {
			filtered = append(filtered, f)
		}
	}
	return filtered, modifiedFiles
}

// toolCallPath 从工具参数中提取路径/模式字段。
func toolCallPath(name string, args json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	for _, k := range []string{"path", "pattern", "glob"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// formatFileOps 把文件清单格式化为摘要尾部块。
func formatFileOps(readFiles, modifiedFiles []string) string {
	if len(readFiles) == 0 && len(modifiedFiles) == 0 {
		return ""
	}
	var b strings.Builder
	if len(readFiles) > 0 {
		b.WriteString("<read-files>\n")
		for _, f := range readFiles {
			b.WriteString("  " + f + "\n")
		}
		b.WriteString("</read-files>\n")
	}
	if len(modifiedFiles) > 0 {
		b.WriteString("<modified-files>\n")
		for _, f := range modifiedFiles {
			b.WriteString("  " + f + "\n")
		}
		b.WriteString("</modified-files>")
	}
	return strings.TrimSpace(b.String())
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
