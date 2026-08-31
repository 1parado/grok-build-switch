package server

// 原生引擎的 server 层装配：Provider 工厂（从当前生效 Profile 构造）、
// 系统提示词与生图工具适配。main.go 按 settings.AgentEngine 选择引擎时调用。

import (
	"context"
	"fmt"
	"os"
	"runtime"
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
		DefaultEffort:           p.DefaultReasoningEffort,
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
// 系统提示词只携带模型无法从对话推断的环境事实（工作目录、目录概貌、
// 平台）；服务方式已由后训练写入权重，不再重复。环境块按会话现算：
// cwd 是每会话状态，不能在服务构造时定死。
func nativeSystemPrompt(env string) string {
	var b strings.Builder
	b.WriteString(`运行在用户的本机，直接读写用户的真实文件系统、执行真实命令。
相对路径基于工作目录解析；沙箱外的路径会被拒绝。
`)
	b.WriteString(env)
	return b.String()
}

// buildEnvSection 环境块：cwd、平台、目录顶层条目。每次 turn 重算，
// 模型由此获得方位感，无需靠 glob 自查身份。
func buildEnvSection(cwd string) string {
	var b strings.Builder
	b.WriteString("\n# 环境\n\n")
	b.WriteString("- 工作目录: " + cwd + "\n")
	b.WriteString("- 平台: " + runtime.GOOS + "/" + runtime.GOARCH + " shell: " + osShell() + "\n")
	if entries := topLevelPreview(cwd); entries != "" {
		b.WriteString("- 目录顶层:\n" + entries)
	}
	return b.String()
}

// osShell 报告用户默认 shell（读 SHELL；空则不猜）。
func osShell() string {
	return os.Getenv("SHELL")
}

// topLevelPreview 列出工作目录顶层条目（目录带尾斜杠，最多 40 条，
// 附加总条目数）。只一层，不递归——Downloads 这类大目录的递归列表
// 会撑爆上下文且全是噪声。
func topLevelPreview(cwd string) string {
	dirEntries, err := os.ReadDir(cwd)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(dirEntries))
	for _, e := range dirEntries {
		// macOS 会往各处塞 .DS_Store；对模型理解目录结构没有价值。
		if e.Name() == ".DS_Store" {
			continue
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	const maxEntries = 40
	shown := names
	truncated := false
	if len(names) > maxEntries {
		shown = names[:maxEntries]
		truncated = true
	}
	var b strings.Builder
	for _, name := range shown {
		b.WriteString("  " + name + "\n")
	}
	suffix := ""
	if truncated {
		suffix = fmt.Sprintf("  …（共 %d 项，已省略）\n", len(names))
	}
	return b.String() + suffix
}

// 编译期引用（保持 import 最小化：agentbridge 用于 Status 语义对齐）。
var _ = agentbridge.Event{}
var _ = agentkit.OriginUser
