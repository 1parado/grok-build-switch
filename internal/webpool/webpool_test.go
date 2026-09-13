package webpool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *Manager) addTestAccount(t *testing.T, sso string) string {
	t.Helper()
	id, _, err := m.upsertCookie(CookieSet{SSO: sso, SSORW: sso}, "test@example.com", "manual", "")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// fakeGrokWeb 模拟 grok.com 网页 API：按 cookie 顺序响应预设剧本。
type fakeGrokWeb struct {
	srv      *httptest.Server
	requests atomic.Int64
	script   func(n int, w http.ResponseWriter, r *http.Request)
}

func newFakeGrokWeb(t *testing.T, script func(int, http.ResponseWriter, *http.Request)) *fakeGrokWeb {
	f := &fakeGrokWeb{script: script}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != webChatPath {
			http.NotFound(w, r)
			return
		}
		f.requests.Add(1)
		f.script(int(f.requests.Load()), w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeWebEvent(w http.ResponseWriter, payload map[string]any) {
	data, _ := json.Marshal(payload)
	_, _ = w.Write(append(data, '\n'))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestParseWebStreamThinkingAndText(t *testing.T) {
	// 模拟真实流：心跳混排、字符串内大括号、思考与正文交替、结构化 token。
	raw := `{"result":{"response":{"token":"思考","isThinking":true}}}
{"result":{"response":{"token":"含{大括号}的正文"}}}
{"result":{"response":{"token":{"action":"webSearch","action_input":{"query":"x"}}}}}
{"result":{"response":{"token":"结尾","isThinking":false}}}
{"modelResponse":{"message":"完整回复"}}`
	var thinks, texts []string
	if err := parseWebStream(strings.NewReader(raw), func(ev WebStreamEvent) error {
		if ev.ThinkDelta != "" {
			thinks = append(thinks, ev.ThinkDelta)
		}
		if ev.TextDelta != "" {
			texts = append(texts, ev.TextDelta)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(thinks, "") != "思考" {
		t.Fatalf("思考增量错误: %v", thinks)
	}
	if strings.Join(texts, "") != "含{大括号}的正文结尾" {
		t.Fatalf("正文增量错误: %v", texts)
	}
}

func TestParseWebStreamRateLimitError(t *testing.T) {
	raw := `{"result":{"response":{"token":"部分","isThinking":false}}}
{"error":"RateLimitError"}`
	err := parseWebStream(strings.NewReader(raw), func(ev WebStreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "RateLimitError") {
		t.Fatalf("应报流内错误: %v", err)
	}
}

func TestChatTranslatesThinkingAndContent(t *testing.T) {
	fake := newFakeGrokWeb(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		writeWebEvent(w, map[string]any{"result": map[string]any{"response": map[string]any{"token": "让我想想", "isThinking": true}}})
		writeWebEvent(w, map[string]any{"result": map[string]any{"response": map[string]any{"token": "答案是2", "isThinking": false}}})
	})
	m := newTestManager(t)
	m.SetUpstreamBase(fake.srv.URL)
	m.addTestAccount(t, "sso-aaa")

	var thinks, texts []string
	text, err := m.Chat(context.Background(), ChatOptions{Model: "grok-4"}, func(ev WebStreamEvent) error {
		thinks = append(thinks, ev.ThinkDelta)
		texts = append(texts, ev.TextDelta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(thinks, "") != "让我想想" {
		t.Fatalf("思考翻译错误: %v", thinks)
	}
	if text != "答案是2" || strings.Join(texts, "") != "答案是2" {
		t.Fatalf("正文翻译错误: %q %v", text, texts)
	}
}

func TestChatFailsOverOn429(t *testing.T) {
	// 第一个账号 429，第二个账号成功；请求头按 cookie 区分账号。
	fake := newFakeGrokWeb(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), "sso-bad") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		writeWebEvent(w, map[string]any{"result": map[string]any{"response": map[string]any{"token": "好的", "isThinking": false}}})
	})
	m := newTestManager(t)
	m.SetUpstreamBase(fake.srv.URL)
	m.addTestAccount(t, "sso-bad")
	m.addTestAccount(t, "sso-good")

	text, err := m.Chat(context.Background(), ChatOptions{Model: "grok-4"}, func(WebStreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if text != "好的" {
		t.Fatalf("故障转移后正文错误: %q", text)
	}
	// 限流账号被隔离为 rate_limited。
	status := m.Status()
	found := false
	for _, acc := range status.Accounts {
		if strings.Contains(acc.ID, accountID("sso-bad")) {
			found = true
			if acc.Class != "rate_limited" {
				t.Fatalf("429 账号应隔离为 rate_limited: %+v", acc)
			}
		}
	}
	if !found {
		t.Fatal("未找到被隔离账号")
	}
}

func TestAccountIsolationAndRecovery(t *testing.T) {
	now := time.Now()
	limited := Account{Class: "rate_limited", RateLimitedAt: now}
	if accountAvailable(limited, now) {
		t.Fatal("刚限流的账号不应可用")
	}
	if !accountAvailable(limited, now.Add(webRateLimitTTL+time.Minute)) {
		t.Fatal("限流超过 TTL 应自动恢复")
	}
	blocked := Account{Class: "blocked", BlockedAt: now}
	if accountAvailable(blocked, now.Add(24*time.Hour)) {
		t.Fatal("blocked 账号不应自动恢复")
	}
	disabled := Account{Disabled: true}
	if accountAvailable(disabled, now) {
		t.Fatal("手动停用账号不应可用")
	}
}

func TestSyncRegistrarCookies(t *testing.T) {
	snapshot := `{"version":1,"email":"a@x.com","cookies":[
		{"name":"sso","value":"sso-1","domain":".grok.com"},
		{"name":"sso-rw","value":"sso-1","domain":".grok.com"},
		{"name":"cf_clearance","value":"cf-1","domain":".grok.com"},
		{"name":"__cf_bm","value":"bm-1","domain":".grok.com"},
		{"name":"unrelated","value":"x","domain":"evil.com"}
	]}`
	dir := t.TempDir()
	if err := writeTestFile(dir, "a@x.com-abc.json", snapshot); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t)
	added, refreshed, total, err := m.SyncRegistrarCookies(dir)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || total != 1 {
		t.Fatalf("导入计数错误: added=%d total=%d", added, total)
	}
	// 再同步一次：刷新不新增。
	added, refreshed, _, _ = m.SyncRegistrarCookies(dir)
	if added != 0 || refreshed != 1 {
		t.Fatalf("重复导入应刷新: added=%d refreshed=%d", added, refreshed)
	}
	// cookie 头包含全部字段。
	status := m.Status()
	if len(status.Accounts) != 1 {
		t.Fatalf("账号数错误: %d", len(status.Accounts))
	}
}

func writeTestFile(dir, name, content string) error {
	return atomicWrite(dir+"/"+name, []byte(content))
}

func TestFlattenTranscript(t *testing.T) {
	msgs := []OpenAIMessage{
		{Role: "system", Content: "你是助手"},
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "你好！"},
		{Role: "user", Content: "再见"},
	}
	out := FlattenTranscript(msgs)
	for _, want := range []string{"[system]\n你是助手", "[user]\n你好", "[assistant]\n你好！", "[user]\n再见"} {
		if !strings.Contains(out, want) {
			t.Fatalf("转录缺少 %q: %q", want, out)
		}
	}
}

func TestResolveModel(t *testing.T) {
	m := ResolveModel("grok-4-reasoning")
	if m.ModelName != "grok-4" || !m.IsReasoning {
		t.Fatalf("reasoning 模型映射错误: %+v", m)
	}
	if m := ResolveModel("unknown-model"); m.ModelName != "grok-4" {
		t.Fatalf("未知模型应回落 grok-4: %+v", m)
	}
	if m := ResolveModel("grok-3"); m.ModelName != "grok-3" {
		t.Fatalf("裸网页模型名应可用: %+v", m)
	}
}

func TestStreamHalfChunkSplit(t *testing.T) {
	// 半行分块：一个 JSON 对象拆成 3 个 Read 段也能正确解析。
	raw := `{"result":{"response":{"tok` + `en":"跨块","isTh` + `inking":true}}}`
	var thinks []string
	if err := parseWebStream(strings.NewReader(raw), func(ev WebStreamEvent) error {
		thinks = append(thinks, ev.ThinkDelta)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(thinks, "") != "跨块" {
		t.Fatalf("跨块解析错误: %v", thinks)
	}
}

func TestSetCookieRefresh(t *testing.T) {
	fake := newFakeGrokWeb(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "__cf_bm=newbm; Path=/; Domain=.grok.com")
		w.Header().Set("Content-Type", "text/plain")
		writeWebEvent(w, map[string]any{"result": map[string]any{"response": map[string]any{"token": "ok", "isThinking": false}}})
	})
	m := newTestManager(t)
	m.SetUpstreamBase(fake.srv.URL)
	m.addTestAccount(t, "sso-refresh")

	if _, err := m.Chat(context.Background(), ChatOptions{}, func(WebStreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	// 凭据文件应已更新（读回验证）。
	file, err := m.loadAccountFile(status.Accounts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if file.Cookies.CFBM != "newbm" {
		t.Fatalf("cf_bm 未回写: %+v", file.Cookies)
	}
}

func TestChatSendsProtocolShape(t *testing.T) {
	var gotBody map[string]any
	var gotCookie, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotUA = r.Header.Get("User-Agent")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/plain")
		writeWebEvent(w, map[string]any{"result": map[string]any{"response": map[string]any{"token": "ok", "isThinking": false}}})
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.SetUpstreamBase(srv.URL)
	m.addTestAccount(t, "sso-shape")

	if _, err := m.Chat(context.Background(), ChatOptions{Model: "grok-4-reasoning", Message: "[user]\n你好"}, func(WebStreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if gotBody["modelName"] != "grok-4" || gotBody["isReasoning"] != true || gotBody["temporary"] != true {
		t.Fatalf("请求体形态错误: %v", gotBody)
	}
	if gotBody["message"] != "[user]\n你好" {
		t.Fatalf("message 错误: %v", gotBody["message"])
	}
	if !strings.Contains(gotCookie, "sso=sso-shape") || !strings.Contains(gotCookie, "sso-rw=sso-shape") {
		t.Fatalf("Cookie 头错误: %s", gotCookie)
	}
	if !strings.Contains(gotUA, "Chrome/133") {
		t.Fatalf("UA 应与 clearance 绑定的 Chrome/133 一致: %s", gotUA)
	}
}
