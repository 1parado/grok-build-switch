package llm

import (
	"strings"
	"testing"
)

func TestProviderForProfileDefaultEffort(t *testing.T) {
	p, err := NewProviderForProfile(ProfileBridge{
		BaseURL:                 "http://127.0.0.1:19999/grok/v1",
		Model:                   "grok-4.6",
		SupportsReasoningEffort: true,
		ReasoningEfforts:        []string{"low", "medium", "high"},
		DefaultEffort:           "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	rp, ok := p.(*ResponsesProvider)
	if !ok {
		t.Fatalf("期望 ResponsesProvider，得到 %T", p)
	}
	if rp.effort != "high" {
		t.Fatalf("Provider 默认档未注入: %q", rp.effort)
	}
	// per-call 覆盖优先于默认档（模拟 run_turn 显式传 low 的场景）。
	body, err := rp.buildRequest("sys", nil, nil, GenerateOptions{Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "" || !strings.Contains(string(body), `"effort":"low"`) {
		t.Fatalf("per-call Effort 应覆盖默认档: %s", body[:200])
	}
}
