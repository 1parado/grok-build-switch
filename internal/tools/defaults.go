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
	}
	return reg
}
