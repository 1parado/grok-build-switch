package permission

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// argOf 从工具参数 JSON 提取匹配字段：bash→command，文件类→path，其余→""。
func argOf(tool string, args json.RawMessage) string {
	var payload map[string]any
	if json.Unmarshal(args, &payload) != nil {
		return ""
	}
	switch tool {
	case "bash":
		if cmd, ok := payload["command"].(string); ok {
			return cmd
		}
	case "read", "write", "edit", "glob", "grep":
		if p, ok := payload["path"].(string); ok && p != "" {
			return p
		}
		if p, ok := payload["pattern"].(string); ok && (tool == "glob") {
			return p
		}
	}
	return ""
}

// matchArg 判断参数是否命中规则参数部分。
//   - 规则无参数部分（裸工具名）：匹配任何参数；
//   - bash：命令前缀通配（"git *" 匹配 "git push -f"，"* rm" 不要求）；
//   - 路径类：glob 匹配（** 跨目录）；参数为空视作不匹配（工具级规则除外）。
func matchArg(rule parsedRule, tool, arg string) bool {
	if !rule.hasArg {
		return true
	}
	pattern := rule.arg
	if pattern == "**" || pattern == "*" {
		return true
	}
	if arg == "" {
		return false
	}
	switch tool {
	case "bash":
		return matchCommand(pattern, arg)
	default:
		return MatchGlob(pattern, arg) || MatchGlob(pattern, filepath.ToSlash(arg))
	}
}

// matchCommand 用通配语义匹配命令行：'*' 匹配任意字符（含空格），
// 其余字符按字面匹配，且整体前缀语义（pattern 比 command 短时按前缀处理）。
func matchCommand(pattern, command string) bool {
	pattern = strings.TrimSpace(pattern)
	command = strings.TrimSpace(command)
	if pattern == "" {
		return false
	}
	// 纯前缀（无通配符）：pattern 是 command 的前缀且停在词边界。
	if !strings.Contains(pattern, "*") {
		if strings.HasPrefix(command, pattern) {
			rest := command[len(pattern):]
			return rest == "" || strings.HasPrefix(rest, " ")
		}
		return false
	}
	// 整串通配（命令不是路径，不按 / 分段）：'*' 匹配任意字符。
	if wildcardMatch(pattern, command) {
		return true
	}
	// "cmd *" 的宽松语义：命令本体恰好等于 cmd（无参数）也视为命中。
	if rest := strings.TrimSuffix(pattern, " *"); rest != pattern {
		return command == strings.TrimSpace(rest)
	}
	return false
}

// MatchGlob 是纯字符串 glob：'*' 匹配一段，'**' 匹配任意（含 '/'），
// '?' 匹配单字符。大小写不敏感（Windows 路径习惯）。
func MatchGlob(pattern, target string) bool {
	return globMatch(strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(strings.TrimSpace(target)))
}

// globMatch 实现 ** 的分段匹配：把 pattern 按 '/' 切分，遇到 '**' 时
// 尝试吞掉任意数量的目标段。
func globMatch(pattern, target string) bool {
	patSegs := splitSegs(pattern)
	tgtSegs := splitSegs(target)
	return segMatch(patSegs, 0, tgtSegs, 0)
}

func splitSegs(s string) []string {
	var out []string
	for _, seg := range strings.Split(s, "/") {
		if seg == "" {
			continue
		}
		out = append(out, seg)
	}
	return out
}

func segMatch(pat []string, pi int, tgt []string, ti int) bool {
	for {
		if pi == len(pat) {
			return ti == len(tgt)
		}
		if pat[pi] == "**" {
			// '**' 尝试吞 0..n 段。
			for k := ti; k <= len(tgt); k++ {
				if segMatch(pat, pi+1, tgt, k) {
					return true
				}
			}
			return false
		}
		if ti == len(tgt) {
			return false
		}
		if !wildcardMatch(pat[pi], tgt[ti]) {
			return false
		}
		pi++
		ti++
	}
}

// wildcardMatch 单段通配：* 与 ? 支持。
func wildcardMatch(pattern, s string) bool {
	px, sx := 0, 0
	starPx, starSx := -1, -1
	for sx < len(s) {
		switch {
		case px < len(pattern) && (pattern[px] == s[sx] || pattern[px] == '?'):
			px++
			sx++
		case px < len(pattern) && pattern[px] == '*':
			starPx = px
			starSx = sx
			px++
		case starPx >= 0:
			px = starPx + 1
			starSx++
			sx = starSx
		default:
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}
