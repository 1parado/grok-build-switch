package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"grok_switch/internal/agentfs"
	"grok_switch/internal/llm"
)

// PlanApprover 是 plan 模式的宿主回调：进入 plan 模式的请求需要用户确认。
// server 层把它接到现有的 plan 审批 WS 事件流。
type PlanApprover interface {
	// RequestPlanMode 由工具调用触发，返回用户决定（approve=进入 plan 模式）。
	RequestPlanMode(ctx context.Context) bool
	// ExitPlanWithPlan 提交计划等待用户批准；返回用户决定与反馈。
	ExitPlanWithPlan(ctx context.Context, plan string) (approved bool, feedback string)
}

// --- enter_plan_mode ---

type EnterPlanModeTool struct{ Approver PlanApprover }

func (EnterPlanModeTool) Name() string { return "enter_plan_mode" }

func (EnterPlanModeTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (EnterPlanModeTool) Doc() string {
	return `进入计划模式：先探索与设计，把实施方案提交用户批准后再动手改代码。
适用于：多文件改动、架构调整、需求有歧义、用户明确要求先出方案。
在计划模式内只读（read/glob/grep），不得修改文件或执行有副作用的命令。`
}

func (t EnterPlanModeTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	if t.Approver == nil {
		return ToolOutput{Text: "已进入计划模式（无审批回调，视为自动同意）。"}
	}
	if t.Approver.RequestPlanMode(ctx) {
		return ToolOutput{Text: "用户同意进入计划模式。请只做只读探索，完成后用 exit_plan_mode 提交方案。"}
	}
	return ToolOutput{Text: "用户拒绝了进入计划模式，请按普通模式继续。", IsError: true}
}

// --- exit_plan_mode ---

type ExitPlanModeTool struct{ Approver PlanApprover }

func (ExitPlanModeTool) Name() string { return "exit_plan_mode" }

func (ExitPlanModeTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"plan": map[string]any{"type": "string", "description": "完整实施计划（markdown）"},
		},
		"required": []string{"plan"},
	}
}

func (ExitPlanModeTool) Doc() string {
	return `提交实施计划并等待用户批准。计划应包含：目标、涉及文件、实施步骤、
验证方式。用户批准后退出计划模式开始实施；被拒绝时按反馈调整。`
}

type exitPlanArgs struct {
	Plan string `json:"plan"`
}

func (t ExitPlanModeTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a exitPlanArgs
	if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Plan) == "" {
		return argHelp("exit_plan_mode", err, `{"plan": "完整计划 markdown"}`)
	}
	if t.Approver == nil {
		return ToolOutput{Text: "计划已提交（无审批回调，视为自动批准）。"}
	}
	approved, feedback := t.Approver.ExitPlanWithPlan(ctx, a.Plan)
	if approved {
		return ToolOutput{Text: "用户批准了计划，退出计划模式，开始实施。"}
	}
	msg := "用户拒绝了计划"
	if feedback != "" {
		msg += "，反馈: " + feedback
	}
	return ToolOutput{Text: msg + "。请按反馈修订后重新提交。", IsError: true}
}

// --- generate_image ---

// ImageGenerator 是生图引擎接口（server 层接 ImagineEngine，测试用假实现）。
type ImageGenerator interface {
	// Generate 生成图片，返回保存路径列表。
	Generate(ctx context.Context, prompt, model, aspect string, count int) ([]string, error)
}

type GenerateImageTool struct{ Engine ImageGenerator }

func (GenerateImageTool) Name() string { return "generate_image" }

func (GenerateImageTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{"type": "string", "description": "图像描述（提示词）"},
			"aspect": map[string]any{"type": "string", "description": "宽高比: 1:1 | 16:9 | 9:16 | 4:3 | 3:4（默认 1:1）"},
			"count":  map[string]any{"type": "integer", "description": "生成数量 1-4（默认 1）"},
		},
		"required": []string{"prompt"},
	}
}

func (GenerateImageTool) Doc() string {
	return `生成图片（走本地生图引擎与账号池）。为获得好结果，提示词应描述主体、
风格、构图与光线。返回保存路径；结果同时出现在画廊。`
}

type genImageArgs struct {
	Prompt string `json:"prompt"`
	Aspect string `json:"aspect"`
	Count  int    `json:"count"`
}

var allowedAspects = map[string]bool{"1:1": true, "16:9": true, "9:16": true, "4:3": true, "3:4": true, "3:2": true, "2:3": true}

func (t GenerateImageTool) Execute(ctx context.Context, args json.RawMessage, env agentfs.Env) ToolOutput {
	var a genImageArgs
	if err := json.Unmarshal(args, &a); err != nil || strings.TrimSpace(a.Prompt) == "" {
		return argHelp("generate_image", err, `{"prompt": "...", "aspect"?: "1:1", "count"?: 1}`)
	}
	if t.Engine == nil {
		return ToolOutput{Text: "生图引擎未启用（设置中可开启生图）", IsError: true}
	}
	aspect := a.Aspect
	if aspect == "" {
		aspect = "1:1"
	}
	if !allowedAspects[aspect] {
		return ToolOutput{Text: fmt.Sprintf("不支持的宽高比 %q，可选: 1:1 16:9 9:16 4:3 3:4 3:2 2:3", aspect), IsError: true}
	}
	count := a.Count
	if count <= 0 {
		count = 1
	}
	if count > 4 {
		count = 4
	}
	paths, err := t.Engine.Generate(ctx, a.Prompt, "", aspect, count)
	if err != nil {
		return ToolOutput{Text: fmt.Sprintf("生图失败: %v", err), IsError: true}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已生成 %d 张图片:\n", len(paths))
	media := make([]llm.ContentPart, 0, len(paths))
	for _, p := range paths {
		b.WriteString(p)
		b.WriteString("\n")
		media = append(media, llm.ImagePart{URI: p, MimeType: "image/jpeg"})
	}
	return ToolOutput{
		Text:  b.String(),
		Media: media,
	}
}
