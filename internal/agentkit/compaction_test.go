package agentkit

import (
	"errors"
	"strings"
	"testing"

	"grok_switch/internal/llm"
)

func userMsg(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: text}}}
}

func assistMsg(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Parts: []llm.ContentPart{llm.TextPart{Text: text}}}
}

func TestShouldCompactThresholds(t *testing.T) {
	cfg := DefaultCompactionConfig()
	if cfg.ShouldCompact(40000, 100000) {
		t.Fatal("低于阈值且低于预留线不应触发")
	}
	if !cfg.ShouldCompact(86000, 100000) {
		t.Fatal("超过 85% 应触发")
	}
	// 预留输出路径：60% + 50k 预留 ≥ 窗口。
	if !cfg.ShouldCompact(60000, 100000) {
		t.Fatal("60% + 预留 50k 应触发")
	}
	if cfg.ShouldCompact(0, 100000) || cfg.ShouldCompact(50000, 0) {
		t.Fatal("未知窗口/零用量不触发")
	}
}

func fakeSummarizer(summary string) func([]llm.Message) (string, error) {
	return func(dropped []llm.Message) (string, error) {
		return summary, nil
	}
}

func TestCompactKeepsUserMessagesAndSummary(t *testing.T) {
	// 序列：任务二超长（装不进预算），一/三/四短。保留 一+三+四，
	// 一与三之间因跳过二而产生省略间隙。
	msgs := []llm.Message{
		userMsg("任务一修复登录"),
		assistMsg("好的，我看一下"),
		userMsg("任务二" + strings.Repeat("细节", 2000)),
		assistMsg("测试已加"),
		userMsg("任务三跑CI"),
		assistMsg("CI 通过"),
		userMsg("任务四交付"),
		assistMsg("收到"),
	}
	origins := []Origin{OriginUser, OriginAssistant, OriginUser, OriginAssistant, OriginUser, OriginAssistant, OriginUser, OriginAssistant}

	// tail 装下最近的短消息；长消息（任务四）装不下，head 保住最老的。
	cfg := CompactionConfig{TriggerRatio: 0.85, KeptUserTokens: 12, HeadUserTokens: 4}
	out, outOrigins, stats, err := Compact(msgs, origins, fakeSummarizer("摘要：之前做了登录修复与测试。"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stats.CompactedCount == 0 {
		t.Fatalf("应有折叠: %+v", stats)
	}
	// 结构：全部保留项都是 user 原话或摘要/省略标记。
	var keptUsers, summaries, elisions int
	for i, m := range out {
		switch outOrigins[i] {
		case OriginUser:
			keptUsers++
			if !strings.HasPrefix(m.Text(), "任务") {
				t.Fatalf("保留的应是用户原话: %q", m.Text())
			}
		case OriginSummary:
			summaries++
			if !strings.HasPrefix(m.Text(), COMPACTION_SUMMARY_PREFIX) {
				t.Fatalf("摘要应带前缀: %q", m.Text())
			}
		case OriginInjection:
			elisions++
			if m.Text() != COMPACTION_ELISION_MARKER {
				t.Fatalf("省略标记错误: %q", m.Text())
			}
		default:
			t.Fatalf("assistant/tool 消息不应保留: %v", outOrigins[i])
		}
	}
	if summaries != 1 {
		t.Fatalf("应恰好 1 条摘要, got %d", summaries)
	}
	if keptUsers < 2 {
		t.Fatalf("head+tail 应至少保留 2 条: %d", keptUsers)
	}
	if elisions < 1 {
		t.Fatalf("head 与 tail 之间应有省略标记: %d", elisions)
	}
	// 最新可保留的用户消息必须在 tail 中（任务四超预算除外，tail 至少含任务三）。
	foundRecent := false
	for _, m := range out {
		if strings.HasPrefix(m.Text(), "任务三") {
			foundRecent = true
		}
	}
	if !foundRecent {
		t.Fatalf("tail 应含任务三: %v", outOrigins)
	}
	// token 收缩。
	if stats.TokensAfter >= stats.TokensBefore {
		t.Fatalf("压实后应更小: %d → %d", stats.TokensBefore, stats.TokensAfter)
	}
}

func TestCompactNoDropped(t *testing.T) {
	msgs := []llm.Message{userMsg("只有一条")}
	origins := []Origin{OriginUser}
	out, outOrigins, stats, err := Compact(msgs, origins, fakeSummarizer("不应调用"), DefaultCompactionConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || outOrigins[0] != OriginUser || stats.CompactedCount != 0 {
		t.Fatalf("无可折叠内容应原样返回: %+v", stats)
	}
}

func TestCompactSummarizerError(t *testing.T) {
	msgs := []llm.Message{userMsg("u1"), assistMsg("a1"), userMsg("u2")}
	origins := []Origin{OriginUser, OriginAssistant, OriginUser}
	_, _, _, err := Compact(msgs, origins, func([]llm.Message) (string, error) {
		return "", errors.New("llm down")
	}, CompactionConfig{KeptUserTokens: 1, HeadUserTokens: 1})
	if err == nil {
		t.Fatal("摘要失败应上抛")
	}
}

func TestCompactMarkersNeverStack(t *testing.T) {
	// 已有摘要与省略标记的消息在下次压实时直接跳过，不进摘要输入。
	summaryMsg := llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: COMPACTION_SUMMARY_PREFIX + "\n旧摘要"}}}
	elision := llm.Message{Role: llm.RoleUser, Parts: []llm.ContentPart{llm.TextPart{Text: COMPACTION_ELISION_MARKER}}}
	msgs := []llm.Message{
		summaryMsg, elision,
		userMsg("新任务一"), assistMsg("ok"),
		userMsg("新任务二"),
	}
	origins := []Origin{OriginSummary, OriginInjection, OriginUser, OriginAssistant, OriginUser}

	var seen []llm.Message
	summarizer := func(dropped []llm.Message) (string, error) {
		seen = dropped
		return "新摘要", nil
	}
	out, outOrigins, _, err := Compact(msgs, origins, summarizer, CompactionConfig{KeptUserTokens: 2, HeadUserTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = out
	// 摘要输入不含旧摘要/标记。
	for _, m := range seen {
		if strings.Contains(m.Text(), "旧摘要") || m.Text() == COMPACTION_ELISION_MARKER {
			t.Fatalf("旧摘要/标记不应进摘要输入: %q", m.Text())
		}
	}
	// 输出恰好一条摘要（没有堆叠）。
	count := 0
	for _, o := range outOrigins {
		if o == OriginSummary {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("摘要不应堆叠: %d", count)
	}
}

func TestCompactInPlaceAndRestore(t *testing.T) {
	mem := NewCtxMemory()
	mem.AppendWithOrigin(userMsg(strings.Repeat("第一轮问题", 100)), OriginUser)
	mem.AppendWithOrigin(assistMsg("a1"), OriginAssistant)
	mem.AppendWithOrigin(userMsg(strings.Repeat("第二轮问题", 100)), OriginUser)

	// 预算只够保留最新一条用户原话。
	msgs, origins := mem.Snapshot()
	out, outOrigins, _, err := Compact(msgs, origins, fakeSummarizer("s"), CompactionConfig{KeptUserTokens: 400, HeadUserTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	mem.CompactInPlace(out, outOrigins, CompactionStats{})
	if mem.UserTurns() != 1 {
		t.Fatalf("压实后轮次错误: %d", mem.UserTurns())
	}
	if len(mem.History()) != len(out) {
		t.Fatal("压实未生效")
	}
}

func TestDropOldest(t *testing.T) {
	msgs := []llm.Message{
		userMsg(strings.Repeat("x", 400)), // ~100 tokens
		userMsg(strings.Repeat("y", 400)),
		userMsg("tail"),
	}
	rest, dropped := DropOldest(msgs, 150)
	if dropped != 1 || len(rest) != 2 {
		t.Fatalf("DropOldest 错误: dropped=%d rest=%d", dropped, len(rest))
	}
	if rest[len(rest)-1].Text() != "tail" {
		t.Fatal("tail 不应被砍")
	}
}

func TestCompactInputText(t *testing.T) {
	dropped := []llm.Message{
		userMsg("用户的问题"),
		assistMsg("回答内容"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c", Name: "read"}}},
		{Role: llm.RoleTool, ToolCallID: "c", Parts: []llm.ContentPart{llm.TextPart{Text: strings.Repeat("结果", 600)}}},
	}
	text := CompactInputText(dropped, 5000)
	if !strings.Contains(text, "user: 用户的问题") || !strings.Contains(text, "tool_call: read") {
		t.Fatalf("摘要输入缺关键内容:\n%s", text)
	}
	// 超长截断。
	long := CompactInputText([]llm.Message{userMsg(strings.Repeat("长", 9000))}, 1000)
	if len(long) > 1100 {
		t.Fatalf("未截断: %d", len(long))
	}
}
