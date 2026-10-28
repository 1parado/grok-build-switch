package tools

import "unicode/utf8"

// prefixText keeps the longest prefix of s that fits in max bytes and ends on
// a UTF-8 boundary. A rune that would be split is left out. max <= 0 yields "".
func prefixText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// suffixText keeps the longest suffix of s that fits in max bytes and starts
// on a UTF-8 boundary. A rune that would be split is left out. max <= 0 yields "".
func suffixText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
