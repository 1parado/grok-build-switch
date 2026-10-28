package permission

import (
	"os"
	"path/filepath"
	"strings"
)

// matchPath applies a path rule. A leading "~" or "~/" is the user home, so
// write(~/.ssh/**) also matches the absolute path under that home. A tilde
// anywhere else stays literal. When base is set, a relative arg is joined to
// it before the comparison, and ".." in an absolute arg is cleaned away.
func matchPath(pattern, arg, base string) bool {
	for _, p := range pathForms(pattern, "") {
		for _, a := range pathForms(arg, base) {
			if MatchGlob(p, a) {
				return true
			}
		}
	}
	return false
}

func pathForms(p, base string) []string {
	p = strings.TrimSpace(p)
	forms := []string{p, filepath.ToSlash(p)}
	add := func(v string) {
		if v == "" {
			return
		}
		forms = append(forms, v, filepath.ToSlash(v))
	}
	if expanded, ok := expandHomePrefix(p); ok {
		add(expanded)
	}
	if filepath.IsAbs(p) {
		if cleaned := filepath.Clean(p); cleaned != p {
			add(cleaned)
		}
	}
	slash := filepath.ToSlash(p)
	if base != "" && p != "" && !filepath.IsAbs(p) && slash != "~" && !strings.HasPrefix(slash, "~/") {
		add(filepath.Clean(filepath.Join(base, p)))
	}
	return forms
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
