package tools

import (
	"grok_switch/internal/agentfs"
)

// DefaultRegistry 组装第一期工具集（设计文档 §6.3 的 9 工具，plan 一对算两个）。
// imageGen 为 nil 时不注册 generate_image；approver 为 nil 时 plan 工具自动同意。
func DefaultRegistry(getEnv func() agentfs.Env, imageGen ImageGenerator, approver PlanApprover, todos *TodoStore) *Registry {
	reg := NewRegistry(func() agentfs.Env { return getEnv() })
	reg.Register(ReadTool{})
	reg.Register(WriteTool{})
	reg.Register(EditTool{})
	reg.Register(GlobTool{})
	reg.Register(GrepTool{})
	reg.Register(BashTool{})
	reg.Register(TodoListTool{Store: todos})
	reg.Register(EnterPlanModeTool{Approver: approver})
	reg.Register(ExitPlanModeTool{Approver: approver})
	if imageGen != nil {
		reg.Register(GenerateImageTool{Engine: imageGen})
		// 单轮调用上限：防模型成功后不停换 prompt 复调烧额度（实测连调 28 次）。
		// 注册表按 turn 重建，计数按 turn 清零；正常多图需求（count≤4/轮内少量复调）不受影响。
		reg.SetCallLimit("generate_image", 4)
	}
	return reg
}
