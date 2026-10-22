package permission

import "strings"

// shellScan splits a command line the way the shell will run it.
// Allow rules must match every segment. A redirect or command substitution
// is not a plain prefix of the first program, so it cannot ride an allow
// rule such as "git *". Deny rules match if any segment or substitution does.
type shellScan struct {
	segments []string
	subs     []string
	redirect bool
	ok       bool
}

func scanShell(command string) shellScan {
	var b strings.Builder
	var segments, subs []string
	inSingle, inDouble := false, false
	redirect := false
	flush := func() {
		seg := strings.TrimSpace(b.String())
		b.Reset()
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if inSingle {
			b.WriteByte(c)
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		if c == '\\' && i+1 < len(command) {
			b.WriteByte(command[i+1])
			i++
			continue
		}
		if c == '\'' && !inDouble {
			inSingle = true
			b.WriteByte(c)
			continue
		}
		if c == '"' {
			inDouble = !inDouble
			b.WriteByte(c)
			continue
		}
		if c == '`' {
			end := strings.IndexByte(command[i+1:], '`')
			if end < 0 {
				return shellScan{ok: false}
			}
			subs = append(subs, command[i+1:i+1+end])
			b.WriteString("``")
			i += end + 1
			continue
		}
		if c == '$' && i+1 < len(command) && command[i+1] == '(' {
			inner, next, ok := scanCommandSubst(command, i+1)
			if !ok {
				return shellScan{ok: false}
			}
			subs = append(subs, inner)
			b.WriteString("$()")
			i = next
			continue
		}
		if !inDouble && (c == '\n' || c == '\r' || c == ';' || c == '|' || c == '&') {
			if (c == '&' || c == '|') && i+1 < len(command) && command[i+1] == c {
				i++
			}
			flush()
			continue
		}
		if !inDouble && (c == '>' || c == '<') {
			redirect = true
			b.WriteByte(c)
			if i+1 < len(command) && (command[i+1] == c || (c == '>' && command[i+1] == '&')) {
				i++
				b.WriteByte(command[i])
			}
			continue
		}
		b.WriteByte(c)
	}
	if inSingle || inDouble {
		return shellScan{ok: false}
	}
	flush()
	return shellScan{segments: segments, subs: subs, redirect: redirect, ok: true}
}

// scanCommandSubst reads the body of a $(...) starting at the '(' byte.
func scanCommandSubst(s string, open int) (inner string, end int, ok bool) {
	depth := 1
	inSingle, inDouble := false, false
	for i := open + 1; i < len(s); i++ {
		c := s[i]
		if inSingle {
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		if c == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if c == '"' {
			inDouble = !inDouble
			continue
		}
		if !inDouble && c == '\'' {
			inSingle = true
			continue
		}
		if inDouble {
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], i, true
			}
		}
	}
	return "", 0, false
}

func unrestricted(pattern string) bool {
	switch strings.TrimSpace(pattern) {
	case "*", "**":
		return true
	default:
		return false
	}
}

// matchBash applies a bash rule to a command line.
//
// Allow matches only when every simple segment matches and the line has no
// redirect or substitution. "git *" therefore allows "git status && git diff"
// and rejects "git status; rm -rf /". An explicit "*" / "**" pattern still
// matches the whole line.
//
// Deny matches when the whole line, any segment, or any substitution body
// matches, so a yolo session cannot hide "rm" behind "echo ok &&".
func matchBash(rule parsedRule, command string) bool {
	if !rule.hasArg {
		return true
	}
	pattern := rule.arg
	if rule.decision == Allow && unrestricted(pattern) {
		return matchCommand(pattern, command)
	}
	scanned := scanShell(command)
	if rule.decision == Deny {
		if matchCommand(pattern, command) {
			return true
		}
		if !scanned.ok {
			return false
		}
		for _, seg := range scanned.segments {
			if matchCommand(pattern, seg) {
				return true
			}
		}
		for _, sub := range scanned.subs {
			if matchBash(rule, sub) {
				return true
			}
		}
		return false
	}
	if !scanned.ok || scanned.redirect || len(scanned.subs) > 0 || len(scanned.segments) == 0 {
		return false
	}
	for _, seg := range scanned.segments {
		if !matchCommand(pattern, seg) {
			return false
		}
	}
	return true
}
