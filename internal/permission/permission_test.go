package permission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	r, err := Parse("Bash(git *)", Allow)
	if err != nil {
		t.Fatal(err)
	}
	if r.tool != "bash" || !r.hasArg || r.arg != "git *" {
		t.Fatalf("解析错误: %+v", r)
	}
	if r.String() != "bash(git *)" {
		t.Fatalf("String 错误: %s", r.String())
	}
	bare, _ := Parse("read", Allow)
	if bare.hasArg {
		t.Fatal("裸工具名不应有参数部分")
	}
	if _, err := Parse("bash(git", Allow); err == nil {
		t.Fatal("未闭合括号应报错")
	}
	if _, err := Parse("(git)", Allow); err == nil {
		t.Fatal("空工具名应报错")
	}
	if _, err := Parse("rm -rf; echo", Allow); err == nil {
		t.Fatal("非法字符应报错（注入防护）")
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, target string
		want            bool
	}{
		{"**", "anything/goes/here.txt", true},
		{"**/*.go", "internal/llm/types.go", true},
		{"**/*.go", "types.go", true},
		{"internal/**", "internal/llm/types.go", true},
		{"internal/**/*.md", "internal/llm/x.md", true},
		{"internal/**", "cmd/main.go", false},
		{"*.go", "sub/types.go", false}, // 单星不跨目录
		{"~/.ssh/**", "~/.ssh/id_ed25519", true},
		{"docs/*", "docs/usage.md", true},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pattern, c.target); got != c.want {
			t.Fatalf("MatchGlob(%q,%q)=%v, want %v", c.pattern, c.target, got, c.want)
		}
	}
}

func TestMatchCommand(t *testing.T) {
	cases := []struct {
		pattern, command string
		want             bool
	}{
		{"git *", "git push -f origin main", true},
		{"git *", "git", true},             // 前缀词边界：'git' 本体也算（rest 为空）
		{"git *", "github-cli run", false}, // 非词边界
		{"git status", "git status --short", true},
		{"git status", "git stash", false},
		{"*rm*", "echo rm-dry", true}, // 通配
		{"npm *", "npm test", true},
	}
	for _, c := range cases {
		r, _ := Parse("bash("+c.pattern+")", Allow)
		if got := matchArg(r, "bash", c.command); got != c.want {
			t.Fatalf("matchCommand(%q,%q)=%v, want %v", c.pattern, c.command, got, c.want)
		}
	}
}

func TestEngineDecisionOrder(t *testing.T) {
	engine, err := NewEngine(filepath.Join(t.TempDir(), "permissions.json"))
	if err != nil {
		t.Fatal(err)
	}
	// allow 规则命中 → allow。
	if err := engine.AddUserRule("read(**)", Allow); err != nil {
		t.Fatal(err)
	}
	if got := engine.Check("read", json.RawMessage(`{"path":"a/b.go"}`)); got != "allow" {
		t.Fatalf("read 应 allow, got %s", got)
	}
	if got := engine.Check("write", json.RawMessage(`{"path":"a.go"}`)); got != "ask" {
		t.Fatalf("未命中应 ask, got %s", got)
	}

	// deny 优先于 allow。
	if err := engine.AddUserRule("write(~/.ssh/**)", Deny); err != nil {
		t.Fatal(err)
	}
	if got := engine.Check("write", json.RawMessage(`{"path":"~/.ssh/id_x"}`)); got != "deny" {
		t.Fatalf("ssh 路径应 deny, got %s", got)
	}
	if reason := engine.DenyReason("write", json.RawMessage(`{"path":"~/.ssh/id_x"}`)); reason == "" {
		t.Fatal("deny reason 不应为空")
	}
	if got := engine.Check("write", json.RawMessage(`{"path":"~/project/a.go"}`)); got != "ask" {
		t.Fatalf("非 ssh 路径仍应 ask, got %s", got)
	}

	// session 规则与 user 规则共存。
	if err := engine.AddSessionRule("grep", Allow); err != nil {
		t.Fatal(err)
	}
	if got := engine.Check("grep", json.RawMessage(`{"pattern":"x"}`)); got != "allow" {
		t.Fatalf("裸工具名规则应匹配任何参数, got %s", got)
	}
	engine.ClearSession()
	if got := engine.Check("grep", json.RawMessage(`{"pattern":"x"}`)); got != "ask" {
		t.Fatalf("清空 session 后应 ask, got %s", got)
	}

	// yolo：只有 deny 能拦。
	engine.SetMode(ModeYolo)
	if got := engine.Check("bash", json.RawMessage(`{"command":"npm install"}`)); got != "allow" {
		t.Fatalf("yolo 应放行, got %s", got)
	}
	if got := engine.Check("write", json.RawMessage(`{"path":"~/.ssh/id_x"}`)); got != "deny" {
		t.Fatalf("yolo 下 deny 仍生效, got %s", got)
	}
	// auto：全放行。
	engine.SetMode(ModeAuto)
	if got := engine.Check("bash", json.RawMessage(`{"command":"rm -rf /"}`)); got != "allow" {
		t.Fatalf("auto 全放行, got %s", got)
	}
	// 回 manual。
	engine.SetMode(ModeManual)
	if got := engine.Check("bash", json.RawMessage(`{"command":"rm -rf /"}`)); got != "ask" {
		t.Fatalf("manual 未命中应 ask, got %s", got)
	}
}

func TestEnginePersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "permissions.json")
	engine, err := NewEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.AddUserRule("bash(git *)", Allow); err != nil {
		t.Fatal(err)
	}
	if err := engine.AddUserRule("bash(rm *)", Deny); err != nil {
		t.Fatal(err)
	}
	// 去重。
	if err := engine.AddUserRule("bash(git *)", Allow); err != nil {
		t.Fatal(err)
	}
	_, userRules := engine.Rules()
	if len(userRules) != 2 {
		t.Fatalf("去重失败: %v", userRules)
	}

	// 重开加载。
	engine2, err := NewEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := engine2.Check("bash", json.RawMessage(`{"command":"git log"}`)); got != "allow" {
		t.Fatalf("重载后 git 应 allow, got %s", got)
	}
	if got := engine2.Check("bash", json.RawMessage(`{"command":"rm -rf x"}`)); got != "deny" {
		t.Fatalf("重载后 rm 应 deny, got %s", got)
	}
	// 移除。
	if err := engine2.RemoveUserRule("bash(rm *)"); err != nil {
		t.Fatal(err)
	}
	if got := engine2.Check("bash", json.RawMessage(`{"command":"rm -rf x"}`)); got != "ask" {
		t.Fatalf("移除后应 ask, got %s", got)
	}
}

func TestEngineCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(path)
	if err != nil {
		t.Fatalf("损坏文件不应致命: %v", err)
	}
	if got := engine.Check("read", nil); got != "ask" {
		t.Fatalf("损坏后应回到默认, got %s", got)
	}
	if _, err := os.Stat(path + ".corrupt.bak"); err != nil {
		t.Fatal("损坏原件应保留备份")
	}
}

func TestBareToolRule(t *testing.T) {
	engine, _ := NewEngine("")
	_ = engine.AddSessionRule("todo_list", Allow)
	// todo_list 参数任意 → allow。
	if got := engine.Check("todo_list", json.RawMessage(`{"items":[]}`)); got != "allow" {
		t.Fatalf("裸规则应放行, got %s", got)
	}
	// 其它工具不受影响。
	if got := engine.Check("bash", json.RawMessage(`{"command":"ls"}`)); got != "ask" {
		t.Fatalf("其它工具应 ask, got %s", got)
	}
}
