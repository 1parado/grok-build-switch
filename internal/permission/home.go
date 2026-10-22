package permission

import (
	"os"
	"path/filepath"
	"strings"
)

// matchPath applies a path rule. A leading "~" or "~/" is the user home, so
// write(~/.ssh/**) also matches the absolute path under that home. A tilde
// anywhere else stays literal. Relative paths are not resolved.
func matchPath(pattern, arg string) bool {
	for _, p := range pathForms(pattern) {
		for _, a := range pathForms(arg) {
			if MatchGlob(p, a) {
				return true
			}
		}
	}
	return false
}

func pathForms(p string) []string {
	forms := []string{p, filepath.ToSlash(p)}
	expanded, ok := expandHomePrefix(p)
	if !ok {
		return forms
	}
	return append(forms, expanded, filepath.ToSlash(expanded))
}

func expandHomePrefix(p string) (string, bool) {
	slash := filepath.ToSlash(strings.TrimSpace(p))
	if slash != "~" && !strings.HasPrefix(slash, "~/") {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", false
	}
	if slash == "~" {
		return filepath.Clean(home), true
	}
	rest := strings.TrimPrefix(slash, "~/")
	return filepath.Clean(filepath.Join(home, filepath.FromSlash(rest))), true
}
