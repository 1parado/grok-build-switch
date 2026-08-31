package server

// 原生引擎的 server 层装配：Provider 工厂（从当前生效 Profile 构造）、
// 系统提示词与生图工具适配。main.go 按 settings.AgentEngine 选择引擎时调用。

import (
	"context"
	"fmt"
	"strings"

	"grok_switch/internal/agentbridge"
	"grok_switch/internal/agentkit"
	"grok_switch/internal/llm"
	"grok_switch/internal/profiles"
	"grok_switch/internal/tools"
)

// NewNativeAgentService 构造原生引擎服务（EngineDeps 绑定到本 Server）。
func (s *Server) NewNativeAgentService() (AgentService, error) {
	return newNativeAgentService(EngineDeps{
		SessionsRoot: s.Paths.DataDir + "/agent2/sessions",
		ProviderFor:  s.nativeProviderFor,
		SystemPrompt: nativeSystemPrompt,
		ImageGen:     s.nativeImageGen(),
		DefaultCwd:   s.agentDefaultCwd(),
	})
}

// nativeProviderFor 从当前生效 Profile 构造 Provider。
// Grok Auth 池 Profile 的 base_url 是 loopback 代理；实际端口与本 Profile
// 记录值可能不同（端口冲突顺延），这里用 ActualPort 重写。
func (s *Server) nativeProviderFor() (llm.Provider, error) {
	if s.Switcher == nil {
		return nil, fmt.Errorf("供应商系统未初始化")
	}
	profile, ok, err := s.Switcher.ActiveStatus()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("没有生效的供应商 Profile")
	}
	bridge := profileToBridge(profile, s.ActualPort)
	return llm.NewProviderForProfile(bridge)
}

// profileToBridge 把 profiles.Profile 投影成 llm.ProfileBridge。
// 模型定义取第一个启用的 [model.*]；没有时用 Profile 级字段。
func profileToBridge(p profiles.Profile, actualPort int) llm.ProfileBridge {
	baseURL := p.BaseURL
	apiKey := p.APIKey
	model := p.DefaultModel
	var contextWindow, maxCompletion int64
	supportsEffort := p.DefaultReasoningEffort != ""
	efforts := []string{"low", "medium", "high"}
	if len(p.Models) > 0 {
		m := p.Models[0]
		if m.BaseURL != "" {
			baseURL = m.BaseURL
		}
		if m.APIKey != "" {
			apiKey = m.APIKey
		}
		if m.Model != "" {
			model = m.Model
		}
		contextWindow = m.ContextWindow
		maxCompletion = m.MaxCompletionTokens
		supportsEffort = m.SupportsReasoningEffort
		if len(m.ReasoningEfforts) > 0 {
			efforts = m.ReasoningEfforts
		}
	}
	// loopback 代理端口跟随实际端口（Profile 里记录的是首选端口）。
	if isLoopbackURL(baseURL) && actualPort > 0 {
		baseURL = rewriteLoopbackPort(baseURL, actualPort)
	}
	return llm.ProfileBridge{
		UpstreamFormat:          p.UpstreamFormat,
		BaseURL:                 baseURL,
		APIKey:                  apiKey,
		Model:                   model,
		SessionKey:              p.ID,
		SupportsReasoningEffort: supportsEffort,
		ReasoningEfforts:        efforts,
		ContextWindow:           contextWindow,
		MaxCompletionTokens:     maxCompletion,
	}
}

func isLoopbackURL(u string) bool {
	return strings.Contains(u, "127.0.0.1") || strings.Contains(u, "localhost")
}

func rewriteLoopbackPort(u string, port int) string {
	idx := strings.Index(u, "://")
	if idx < 0 {
		return u
	}
	scheme := u[:idx+3]
	rest := u[idx+3:]
	slash := strings.Index(rest, "/")
	host := rest
	path := ""
	if slash >= 0 {
		host, path = rest[:slash], rest[slash:]
	}
	colon := strings.LastIndex(host, ":")
	if colon >= 0 {
		host = host[:colon]
	}
	return fmt.Sprintf("%s%s:%d%s", scheme, host, port, path)
}

// nativeImageGen 返回生图工具适配；生图全局开关关闭或号池无账号时返回 nil
// （工具不注册，模型回到无生图状态，与 acp 引擎的 MCP 移除语义一致）。
// Registry 每 turn 重建，开关切换即时生效。
func (s *Server) nativeImageGen() tools.ImageGenerator {
	if s.Settings != nil {
		if current, err := s.Settings.Get(); err != nil || !current.ImageGenEnabled {
			return nil
		}
	}
	if s.Imagine == nil || s.Imagine.AccountCount() == 0 {
		return nil
	}
	return &imagineToolAdapter{server: s}
}

// imagineToolAdapter 把 ImagineEngine 适配成 tools.ImageGenerator。
type imagineToolAdapter struct{ server *Server }

func (a *imagineToolAdapter) Generate(ctx context.Context, prompt, model, aspect string, count int) ([]string, error) {
	var paths []string
	for i := 0; i < count; i++ {
		res := a.server.Imagine.Generate(ctx, prompt, model, aspect)
		if !res.OK || len(res.Images) == 0 {
			if i == 0 {
				return nil, fmt.Errorf("%s", orEmpty(res.ErrMsg, "生图失败"))
			}
			break
		}
		paths = append(paths, res.Images...)
	}
	return paths, nil
}

func (s *Server) agentDefaultCwd() string {
	if s.Settings == nil {
		return ""
	}
	current, err := s.Settings.Get()
	if err != nil {
		return ""
	}
	return current.AgentDefaultCwd
}

// nativeSystemPrompt 组装原生引擎系统提示词。
// 骨架借鑑 Kimi 内置工具文档的组织方式（§6.3）；文案用中文，
// 与本工具用户群一致。toolDoc 由注册表汇总（含每个工具的用法契约）。
func nativeSystemPrompt(toolDoc string) string {
	var b strings.Builder
	b.WriteString(`你是 grok_switch 内置的软件工程 Agent，运行在用户的本机，直接操作他们的项目。

# 工作方式

- 先理解再动手：改动前读相关代码，确认最近的结构与约定；不确定时先问。
- 从事实出发：以代码为准，不要凭文档或猜测断言。
- 改动聚焦：只做当前任务要求的事，不顺手重构。
- 验证闭环：改完代码后运行构建/测试验证；无法验证时明确说明。

# 工具使用

- 能用专用工具就不要用 bash：读文件用 read、改文件用 edit/write、找文件用 glob、搜内容用 grep。
- 多个独立的只读操作放在同一轮并行发起。
- 长命令（构建、测试、安装依赖）设置合理的 timeout。
- 危险操作（删除、覆盖用户数据、全局安装）先说明再执行。

# 输出

- 用用户使用的语言回复。
- 解释改了什么、为什么这么改；引用代码给出路径与行号。
- 遇到阻塞如实说明，不要编造结果。

`)
	b.WriteString(toolDoc)
	return b.String()
}

// 编译期引用（保持 import 最小化：agentbridge 用于 Status 语义对齐）。
var _ = agentbridge.Event{}
var _ = agentkit.OriginUser
