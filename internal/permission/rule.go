// Package permission 是原生引擎的权限规则引擎（设计文档 D5）。
//
// 规则 DSL：`Tool(pattern)`，如
//
//	read(**)          允许读任何文件
//	bash(git *)       允许 git 只读命令（前缀匹配）
//	bash(rm *)        拒绝递归删除
//	write(**)         允许写任何文件
//	grep              裸工具名 = 任意参数都匹配
//
// 决策顺序：deny 规则 > 模式(yolo/auto) > allow 规则 > 默认 ask。
// 两级 scope：session（内存，重启即失）与 user（permissions.json 持久）。
package permission

import (
	"fmt"
	"strings"
)

// Decision 是规则决定。
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
)

// Mode 是顶层权限姿态（对齐 Kimi manual/yolo/auto）。
type Mode string

const (
	// ModeManual：规则驱动，未命中 ask（默认）。
	ModeManual Mode = "manual"
	// ModeYolo：只有 deny 规则能拦。
	ModeYolo Mode = "yolo"
	// ModeAuto：全放行（等价旧桥的 session_auto_approve）。
	ModeAuto Mode = "auto"
)

// Rule 是一条权限规则。
type Rule struct {
	Decision Decision `json:"decision"`
	// Pattern 是 DSL 形态：`Tool(argPattern)` 或裸 `Tool`。
	Pattern string `json:"pattern"`
	// Reason 拒绝时回给模型的说明（可选）。
	Reason string `json:"reason,omitempty"`
}

// parsedRule 是编译后的规则。
type parsedRule struct {
	decision Decision
	tool     string
	arg      string // 空串 = 不看参数
	reason   string
	hasArg   bool
}

// Parse 解析 DSL。合法形态：
//   - "read"              → 工具级
//   - "read(**)"          → 工具 + 参数 glob
//   - "Bash(git *)"       → 工具名大小写不敏感
func Parse(pattern string, decision Decision) (parsedRule, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return parsedRule{}, fmt.Errorf("permission: 空规则")
	}
	if invalid := invalidChars(p); invalid {
		return parsedRule{}, fmt.Errorf("permission: 规则含非法字符 %q", p)
	}
	tool := p
	arg := ""
	hasArg := false
	if open := strings.Index(p, "("); open >= 0 {
		if !strings.HasSuffix(p, ")") {
			return parsedRule{}, fmt.Errorf("permission: 规则括号不闭合 %q", p)
		}
		tool = p[:open]
		arg = p[open+1 : len(p)-1]
		hasArg = true
	}
	tool = strings.ToLower(strings.TrimSpace(tool))
	if tool == "" {
		return parsedRule{}, fmt.Errorf("permission: 规则缺工具名 %q", p)
	}
	if decision != Allow && decision != Deny {
		return parsedRule{}, fmt.Errorf("permission: 非法决定 %q", decision)
	}
	return parsedRule{decision: decision, tool: tool, arg: strings.TrimSpace(arg), hasArg: hasArg, reason: ""}, nil
}

func invalidChars(p string) bool {
	// DSL 只允许工具名字符、glob 通配与括号空格。
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '*', r == '(', r == ')', r == ' ',
			r == '/', r == '.', r == '~', r == '?', r == '[', r == ']':
		default:
			return true
		}
	}
	return false
}

// String 还原 DSL 形态（带参数时）。
func (r parsedRule) String() string {
	if r.hasArg {
		return fmt.Sprintf("%s(%s)", r.tool, r.arg)
	}
	return r.tool
}
