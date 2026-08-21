package agentbridge

import (
	"fmt"
	"strings"
)

const (
	HistoryBootstrapMaxMsgs     = 16
	HistoryBootstrapPerMsgChars = 2000
	HistoryBootstrapMaxChars    = 14000
)

// BuildHistoryBootstrap builds a one-shot continuity preamble for the agent when
// session/load failed and a fresh session was created. The UI already shows the
// full transcript; this text is only injected into the first prompt.
func BuildHistoryBootstrap(messages []HistoryMessage) string {
	if len(messages) == 0 {
		return ""
	}
	var selected []HistoryMessage
	for i := len(messages) - 1; i >= 0 && len(selected) < HistoryBootstrapMaxMsgs; i-- {
		msg := messages[i]
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		selected = append(selected, HistoryMessage{Role: role, Content: content})
	}
	if len(selected) == 0 {
		return ""
	}
	// Reverse to chronological order.
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}

	var b strings.Builder
	b.WriteString("【会话连续性摘要 — 仅供上下文，请勿复述全文或重新问候】\n")
	b.WriteString("以下是此前对话的压缩摘录。请只回答用户本条新消息。\n\n")
	bodyBudget := HistoryBootstrapMaxChars
	omitted := 0
	for _, msg := range selected {
		label := "User"
		if msg.Role == "assistant" {
			label = "Assistant"
		}
		text := softTruncate(msg.Content, HistoryBootstrapPerMsgChars)
		block := fmt.Sprintf("### %s\n%s\n\n", label, text)
		if len(block) > bodyBudget {
			omitted++
			continue
		}
		b.WriteString(block)
		bodyBudget -= len(block)
	}
	if omitted > 0 {
		b.WriteString(fmt.Sprintf("…（另有 %d 轮因长度限制未纳入摘要）\n\n", omitted))
	}
	b.WriteString("—— 摘要结束 ——\n")
	return b.String()
}

func softTruncate(text string, max int) string {
	text = strings.TrimSpace(text)
	if max <= 0 || len(text) <= max {
		return text
	}
	// Prefer rune-safe cut near max.
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…（已截断）"
}

// CountUserTurns returns how many user messages appear in a transcript.
func CountUserTurns(messages []HistoryMessage) int {
	n := 0
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "user") && strings.TrimSpace(msg.Content) != "" {
			n++
		}
	}
	return n
}

// DropLastUserRewindIndex mirrors grok-app: keep turns before the last user
// message. With N user turns, target is N-2 (or 0 when N<=1).
func DropLastUserRewindIndex(userTurnCount int) int {
	if userTurnCount <= 1 {
		return 0
	}
	return userTurnCount - 2
}
