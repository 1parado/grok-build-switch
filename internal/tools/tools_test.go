package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"grok_switch/internal/agentfs"
	"grok_switch/internal/llm"
)

func testEnv(t *testing.T) agentfs.Env {
	t.Helper()
	return agentfs.Env{Cwd: t.TempDir()}
}

func runTool(t *testing.T, tool Tool, args string, env agentfs.Env) ToolOutput {
	t.Helper()
	out := tool.Execute(context.Background(), json.RawMessage(args), env)
	return out
}

func TestReadTool(t *testing.T) {
	env := testEnv(t)
	path := filepath.Join(env.Cwd, "sample.go")
	_ = env.WriteText(path, "package main\n\nimport \"fmt\"\n\nfunc main() {}\n")

	out := runTool(t, ReadTool{}, `{"path": "sample.go"}`, env)
	if out.IsError {
		t.Fatalf("read 失败: %s", out.Text)
	}
	if !strings.Contains(out.Text, "1\tpackage main") || !strings.Contains(out.Text, "5\tfunc main() {}") {
		t.Fatalf("行号格式错误:\n%s", out.Text)
	}

	// 分页。
	out = runTool(t, ReadTool{}, `{"path": "sample.go", "offset": 3, "limit": 2}`, env)
	if !strings.Contains(out.Text, "3\timport") || strings.Contains(out.Text, "4\t") && strings.Contains(out.Text, "5\t") && !strings.Contains(out.Text, "继续读") {
		t.Fatalf("分页读取错误:\n%s", out.Text)
	}

	// 未命中文件。
	out = runTool(t, ReadTool{}, `{"path": "nope.txt"}`, env)
	if !out.IsError {
		t.Fatal("缺失文件应报错")
	}

	// 越界行。
	out = runTool(t, ReadTool{}, `{"path": "sample.go", "offset": 99}`, env)
	if !out.IsError {
		t.Fatal("超起始行应报错")
	}
}

func TestWriteAndEditTool(t *testing.T) {
	env := testEnv(t)

	out := runTool(t, WriteTool{}, `{"path": "a.txt", "content": "line1\nline2\nline3"}`, env)
	if out.IsError {
		t.Fatalf("write 失败: %s", out.Text)
	}

	out = runTool(t, EditTool{}, `{"path": "a.txt", "old_string": "line2", "new_string": "middle"}`, env)
	if out.IsError {
		t.Fatalf("edit 失败: %s", out.Text)
	}
	got, _ := env.ReadText(filepath.Join(env.Cwd, "a.txt"), 0)
	if got != "line1\nmiddle\nline3" {
		t.Fatalf("edit 结果错误: %q", got)
	}

	// 多处命中拒绝。
	_ = env.WriteText(filepath.Join(env.Cwd, "b.txt"), "x\nx\n")
	out = runTool(t, EditTool{}, `{"path": "b.txt", "old_string": "x", "new_string": "y"}`, env)
	if !out.IsError || !strings.Contains(out.Text, "2 处") {
		t.Fatalf("多处命中应拒绝: %+v", out)
	}

	// 未命中提示。
	out = runTool(t, EditTool{}, `{"path": "a.txt", "old_string": "不存在", "new_string": "y"}`, env)
	if !out.IsError || !strings.Contains(out.Text, "未命中") {
		t.Fatalf("未命中应提示: %+v", out)
	}
}

func TestGlobTool(t *testing.T) {
	env := testEnv(t)
	_ = env.WriteText(filepath.Join(env.Cwd, "a.go"), "package a\n")
	_ = env.WriteText(filepath.Join(env.Cwd, "sub", "b.go"), "package b\n")
	_ = env.WriteText(filepath.Join(env.Cwd, "sub", "deep", "c.md"), "# c\n")

	out := runTool(t, GlobTool{}, `{"pattern": "**/*.go"}`, env)
	if out.IsError {
		t.Fatalf("glob 失败: %s", out.Text)
	}
	if !strings.Contains(out.Text, "a.go") || !strings.Contains(out.Text, filepath.Join("sub", "b.go")) {
		t.Fatalf("** 匹配错误:\n%s", out.Text)
	}
	if strings.Contains(out.Text, "c.md") {
		t.Fatalf("不应匹配 md: %s", out.Text)
	}

	out = runTool(t, GlobTool{}, `{"pattern": "sub/**/*.md"}`, env)
	if !strings.Contains(out.Text, "c.md") {
		t.Fatalf("sub/**/*.md 匹配失败:\n%s", out.Text)
	}
}

func TestGrepTool(t *testing.T) {
	env := testEnv(t)
	_ = env.WriteText(filepath.Join(env.Cwd, "code.go"), "func Alpha() {}\nfunc Beta() int { return Alpha() }\n")
	_ = env.WriteText(filepath.Join(env.Cwd, ".git", "ignored.go"), "func Alpha() {}\n")
	_ = env.WriteText(filepath.Join(env.Cwd, "skip.txt"), "Alpha in txt\n")

	out := runTool(t, GrepTool{}, `{"pattern": "Alpha"}`, env)
	if out.IsError {
		t.Fatalf("grep 失败: %s", out.Text)
	}
	if !strings.Contains(out.Text, "code.go") || !strings.Contains(out.Text, "1: func Alpha() {}") {
		t.Fatalf("命中格式错误:\n%s", out.Text)
	}
	if strings.Contains(out.Text, ".git") {
		t.Fatalf("应跳过 .git:\n%s", out.Text)
	}

	// glob 过滤。
	out = runTool(t, GrepTool{}, `{"pattern": "Alpha", "glob": "*.txt"}`, env)
	if !strings.Contains(out.Text, "skip.txt") || strings.Contains(out.Text, "code.go") {
		t.Fatalf("glob 过滤错误:\n%s", out.Text)
	}

	// 无效正则。
	out = runTool(t, GrepTool{}, `{"pattern": "([bad"}`, env)
	if !out.IsError {
		t.Fatal("无效正则应报错")
	}
}

func TestBashTool(t *testing.T) {
	env := testEnv(t)

	out := runTool(t, BashTool{}, `{"command": "echo engine-bash"}`, env)
	if out.IsError || !strings.Contains(out.Text, "engine-bash") {
		t.Fatalf("bash 失败: %+v", out)
	}

	// 非零退出码。
	out = runTool(t, BashTool{}, `{"command": "exit 7"}`, env)
	if !out.IsError || !strings.Contains(out.Text, "7") {
		t.Fatalf("非零退出码错误: %+v", out)
	}

	// 超时。
	out = runTool(t, BashTool{}, `{"command": "sleep 3", "timeout": 1}`, env)
	if !out.IsError || !strings.Contains(out.Text, "超时") {
		t.Fatalf("超时错误: %+v", out)
	}

	// dir 参数。
	sub := filepath.Join(env.Cwd, "subdir")
	_ = os.MkdirAll(sub, 0o755)
	out = runTool(t, BashTool{}, `{"command": "pwd", "dir": "subdir"}`, env)
	if out.IsError || !strings.Contains(out.Text, "subdir") {
		t.Fatalf("dir 参数错误: %+v", out)
	}

	// 越界 dir。
	out = runTool(t, BashTool{}, `{"command": "pwd", "dir": "../../etc"}`, env)
	if !out.IsError {
		t.Fatal("越界 dir 应拒绝")
	}
}

func TestTodoListTool(t *testing.T) {
	store := &TodoStore{}
	tool := TodoListTool{Store: store}

	out := runTool(t, tool, `{"items": [{"content": "step1", "status": "completed"}, {"content": "step2", "status": "in_progress"}, {"content": "step3", "status": "weird"}]}`, agentfs.Env{})
	if out.IsError {
		t.Fatalf("todo 失败: %s", out.Text)
	}
	items := store.Snapshot()
	if len(items) != 3 {
		t.Fatalf("应有 3 项: %+v", items)
	}
	if items[2].Status != "pending" {
		t.Fatalf("非法状态应归一为 pending: %+v", items[2])
	}
}

type autoApprover struct{}

func (autoApprover) RequestPlanMode(ctx context.Context) bool { return true }
func (autoApprover) ExitPlanWithPlan(ctx context.Context, plan string) (bool, string) {
	return true, ""
}

func TestPlanTools(t *testing.T) {
	env := testEnv(t)
	out := runTool(t, EnterPlanModeTool{Approver: autoApprover{}}, `{}`, env)
	if out.IsError {
		t.Fatalf("enter plan 失败: %s", out.Text)
	}
	out = runTool(t, ExitPlanModeTool{Approver: autoApprover{}}, `{"plan": "# 计划\n1. 做事"}`, env)
	if out.IsError {
		t.Fatalf("exit plan 失败: %s", out.Text)
	}

	// 无参数校验。
	out = runTool(t, ExitPlanModeTool{Approver: autoApprover{}}, `{}`, env)
	if !out.IsError {
		t.Fatal("空 plan 应报错")
	}
}

type fakeImageGen struct{}

func (fakeImageGen) Generate(ctx context.Context, prompt, model, aspect string, count int) ([]string, error) {
	return []string{"/tmp/gallery/a.png", "/tmp/gallery/b.png"}, nil
}

func TestGenerateImageTool(t *testing.T) {
	env := testEnv(t)
	tool := GenerateImageTool{Engine: fakeImageGen{}}

	out := runTool(t, tool, `{"prompt": "一只猫"}`, env)
	if out.IsError || !strings.Contains(out.Text, "已生成 2 张") {
		t.Fatalf("生图失败: %+v", out)
	}

	out = runTool(t, tool, `{"prompt": "x", "aspect": "21:9"}`, env)
	if !out.IsError {
		t.Fatal("非法宽高比应拒绝")
	}

	// 引擎未启用。
	out = runTool(t, GenerateImageTool{}, `{"prompt": "x"}`, env)
	if !out.IsError {
		t.Fatal("无引擎应报错")
	}
}

// --- Registry / Adapter 集成 ---

func TestRegistrySchemasAndExecute(t *testing.T) {
	env := testEnv(t)
	reg := DefaultRegistry(func() agentfs.Env { return env }, nil, nil, &TodoStore{})

	names := reg.Names()
	if len(names) != 9 {
		t.Fatalf("默认注册 9 工具, got %d: %v", len(names), names)
	}
	schemas := reg.Schemas()
	if len(schemas) != 9 {
		t.Fatalf("schema 数错误: %d", len(schemas))
	}
	for _, s := range schemas {
		if s.Name == "" || s.Description == "" || s.Schema == nil {
			t.Fatalf("schema 字段缺失: %+v", s)
		}
	}
	// 未知工具。
	out := reg.ExecuteTool(context.Background(), mustCall("nope", "{}"))
	if !out.IsError {
		t.Fatal("未知工具应报错")
	}
	// read 走注册表全链路。
	_ = env.WriteText(filepath.Join(env.Cwd, "x.txt"), "hello")
	out = reg.ExecuteTool(context.Background(), mustCall("read", `{"path":"x.txt"}`))
	if out.IsError || !strings.Contains(out.Text, "hello") {
		t.Fatalf("注册表 read 失败: %+v", out)
	}
	// SystemPromptDoc。
	doc := reg.SystemPromptDoc()
	if !strings.Contains(doc, "## read") || !strings.Contains(doc, "## bash") {
		t.Fatalf("system prompt 文档缺失:\n%s", doc)
	}
}

func TestRegistryUnregisterImageGen(t *testing.T) {
	env := testEnv(t)
	reg := DefaultRegistry(func() agentfs.Env { return env }, fakeImageGen{}, nil, &TodoStore{})
	if len(reg.Names()) != 10 {
		t.Fatalf("带生图应 10 工具: %d", len(reg.Names()))
	}
	reg.Unregister("generate_image")
	if len(reg.Names()) != 9 {
		t.Fatalf("注销后应 9 工具: %d", len(reg.Names()))
	}
	out := reg.ExecuteTool(context.Background(), mustCall("generate_image", `{"prompt":"x"}`))
	if !out.IsError {
		t.Fatal("注销后调用应报未知工具")
	}
}

func TestAdapterBudgetTruncation(t *testing.T) {
	env := testEnv(t)
	// 生成一个 1000 行、每行 60 字符的文件：read 无分页全读时
	// 输出超 ResultBudget，触发注册表的截断路径。
	var b strings.Builder
	for i := 0; i < 1000; i++ {
		b.WriteString(strings.Repeat("x", 60))
		b.WriteString("\n")
	}
	_ = env.WriteText(filepath.Join(env.Cwd, "big.txt"), b.String())
	reg := NewRegistry(func() agentfs.Env { return env })
	reg.Register(ReadTool{})

	adapter := NewToolsAdapter(reg)
	res, err := adapter.Execute(context.Background(), mustCall("read", `{"path":"big.txt","limit":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatalf("超预算应标记 truncated, len=%d", len(res.Output))
	}
	if len(res.Output) > ResultBudget+100 {
		t.Fatalf("输出未截断: %d", len(res.Output))
	}
}

func mustCall(name, args string) llm.ToolCall {
	return llm.ToolCall{ID: "t-" + name, Name: name, Arguments: json.RawMessage(args)}
}
