package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// McpServerConfig 描述一个写入 config.toml 的 [mcp_servers.<name>] 条目。
type McpServerConfig struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
}

// EnsureMcpServerToFile 确保 config.toml 中存在 [mcp_servers.<name>] 配置，
// 让 Grok CLI 以原生 MCP 机制加载该服务器（命令或参数变化时自动更新）。
// 幂等：内容一致时不写盘。未知段（含已有 [mcp_servers] 其它条目）保留。
func EnsureMcpServerToFile(path string, cfg McpServerConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		data = []byte{}
	}
	next := EnsureMcpServerText(data, cfg)
	if string(next) == string(data) {
		return nil
	}
	if err := atomicWrite(path, next); err != nil {
		return err
	}
	invalidateDocCache()
	return nil
}

// RemoveMcpServerToFile 从 config.toml 删除 [mcp_servers.<name>] 段
// （生图能力关闭时移除注册，让模型回到无生图工具的原始状态）。
func RemoveMcpServerToFile(path, name string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	next := RemoveMcpServerText(data, name)
	if string(next) == string(data) {
		return nil
	}
	if err := atomicWrite(path, next); err != nil {
		return err
	}
	invalidateDocCache()
	return nil
}

// RemoveMcpServerText 是 RemoveMcpServerToFile 的纯文本实现。
func RemoveMcpServerText(data []byte, name string) []byte {
	header := "mcp_servers." + name
	lines := splitLines(string(data))
	var out []string
	for i := 0; i < len(lines); {
		if parseHeader(lines[i]) == header {
			i = skipSection(lines, i+1)
			continue
		}
		out = append(out, lines[i])
		i++
	}
	result := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if result == "" {
		return []byte{}
	}
	return []byte(result + "\n")
}

// skipSectionWithSubtables 返回 header 段及其全部子表（[a.b] 之下还有
// [a.b.c]）的结束下标。skipSection 只认下一个 [header] 行，会把
// [mcp_servers.x.env] 这类子表留在段外——替换主体后与段内 inline table
// 同 key 共存，产生 TOML 解析错误（"key env should be a table, not a
// value"）。
func skipSectionWithSubtables(lines []string, start int, header string) int {
	end := skipSection(lines, start)
	prefix := header + "."
	for end < len(lines) {
		next := parseHeader(lines[end])
		if !strings.HasPrefix(next, prefix) {
			break
		}
		end = skipSection(lines, end+1)
	}
	return end
}

// EnsureMcpServerText 是 EnsureMcpServerToFile 的纯文本实现。
// 幂等：已存在匹配段时更新它；重复段（异常情况）只保留第一个。
// 同时清理 grok_switch 曾用过的旧服务器名（仅当命令指向同一可执行文件，
// 避免误删用户自己的同名配置）。
func EnsureMcpServerText(data []byte, cfg McpServerConfig) []byte {
	header := "mcp_servers." + cfg.Name
	legacy := "mcp_servers.grok_switch_imagine"
	lines := splitLines(string(data))
	var out []string
	replaced := false
	for i := 0; i < len(lines); {
		current := parseHeader(lines[i])
		if current == header {
			end := skipSectionWithSubtables(lines, i+1, current)
			// 已写入过（含重复段）则丢弃，保证最终只有一个。
			if !replaced {
				existing := strings.Join(lines[i:end], "\n")
				if mcpSectionEqual(existing, cfg) {
					out = append(out, lines[i:end]...)
				} else {
					out = append(out, buildMcpSection(cfg)...)
				}
				replaced = true
			}
			i = end
			continue
		}
		if legacy != "" && legacy != header && current == legacy {
			end := skipSectionWithSubtables(lines, i+1, current)
			existing := strings.Join(lines[i:end], "\n")
			if mcpSectionCommandEquals(existing, cfg.Command) {
				// 旧名残留且指向同一可执行文件：迁移清理。
				i = end
				continue
			}
			out = append(out, lines[i:end]...)
			i = end
			continue
		}
		out = append(out, lines[i])
		i++
	}
	if !replaced {
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, buildMcpSection(cfg)...)
	}
	result := strings.TrimRight(strings.Join(out, "\n"), "\n")
	if result == "" {
		return []byte{}
	}
	return []byte(result + "\n")
}

// mcpSectionCommandEquals 判断一个 [mcp_servers.*] 段的 command 是否与给定
// 命令一致（用于迁移清理判断）。
func mcpSectionCommandEquals(sectionText, command string) bool {
	var doc map[string]any
	if err := toml.Unmarshal([]byte(sectionText), &doc); err != nil {
		return false
	}
	table, ok := doc["mcp_servers"].(map[string]any)
	if !ok {
		return false
	}
	for _, v := range table {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := entry["command"].(string); cmd == command {
			return true
		}
	}
	return false
}

// buildMcpSection 生成 [mcp_servers.<name>] 的 TOML 文本。
func buildMcpSection(cfg McpServerConfig) []string {
	args := make([]string, 0, len(cfg.Args))
	for _, a := range cfg.Args {
		args = append(args, quote(a))
	}
	env := make([]string, 0, len(cfg.Env))
	for k, v := range cfg.Env {
		env = append(env, quote(k)+" = "+quote(v))
	}
	lines := []string{"[" + "mcp_servers." + cfg.Name + "]", "command = " + quote(cfg.Command)}
	if len(args) > 0 {
		lines = append(lines, "args = ["+strings.Join(args, ", ")+"]")
	}
	if len(env) > 0 {
		lines = append(lines, "env = { "+strings.Join(env, ", ")+" }")
	}
	lines = append(lines, "enabled = true")
	return lines
}

// mcpSectionEqual 判断已有 section 文本与期望配置是否一致（只比较
// command / args / env / enabled，忽略注释与空白差异）。
func mcpSectionEqual(existing string, cfg McpServerConfig) bool {
	doc, err := parseTomlLines(existing)
	if err != nil {
		return false
	}
	table, ok := doc["mcp_servers"].(map[string]any)
	if !ok {
		return false
	}
	entry, ok := table[cfg.Name].(map[string]any)
	if !ok {
		return false
	}
	if cmd, _ := entry["command"].(string); cmd != cfg.Command {
		return false
	}
	if enabled, _ := entry["enabled"].(bool); !enabled {
		return false
	}
	if args, ok := entry["args"].([]any); ok {
		if len(args) != len(cfg.Args) {
			return false
		}
		for i, a := range args {
			s, _ := a.(string)
			if s != cfg.Args[i] {
				return false
			}
		}
	} else if len(cfg.Args) > 0 {
		return false
	}
	if env, ok := entry["env"].(map[string]any); ok {
		if len(env) != len(cfg.Env) {
			return false
		}
		for k, v := range cfg.Env {
			if got, _ := env[k].(string); got != v {
				return false
			}
		}
	} else if len(cfg.Env) > 0 {
		return false
	}
	return true
}

// parseTomlLines 仅用于 mcpSectionEqual 的轻量解析（复用 go-toml）。
func parseTomlLines(text string) (map[string]any, error) {
	doc := map[string]any{}
	if strings.TrimSpace(text) == "" {
		return doc, nil
	}
	if err := toml.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse mcp section: %w", err)
	}
	return doc, nil
}
