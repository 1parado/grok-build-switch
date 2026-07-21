package registrar

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func (s *Service) Probe(config *Config) ProbeResult {
	s.mu.Lock()
	current := s.config
	s.mu.Unlock()
	if config != nil {
		current = normalizeConfig(*config)
	}
	browser := strings.TrimSpace(current.BrowserPath)
	if browser == "" {
		browser = findBrowser()
	}
	browserOK := false
	if browser != "" {
		if stat, err := os.Stat(browser); err == nil && !stat.IsDir() {
			browserOK = true
		}
	}
	checks := []ProbeCheck{
		{Name: "browser", OK: browserOK, Required: true, Detail: firstNonEmpty(browser, "未发现 Chrome / Chromium / Edge")},
		{Name: "email_provider", OK: current.EmailProvider == "hotmail" || current.EmailProvider == "cloudmail" || current.EmailProvider == "cloudflare", Required: true, Detail: current.EmailProvider},
		{Name: "auth_dir", OK: s.resolvedAuthDir() != "", Required: true, Detail: s.resolvedAuthDir()},
	}
	if err := validateConfig(current, true); err != nil {
		checks = append(checks, ProbeCheck{Name: "provider_config", OK: false, Required: true, Detail: err.Error()})
	} else {
		checks = append(checks, ProbeCheck{Name: "provider_config", OK: true, Required: true, Detail: "邮箱配置完整"})
	}
	result := ProbeResult{OK: true, Checks: checks}
	for _, check := range checks {
		if check.Required && !check.OK {
			result.OK = false
		}
	}
	return result
}

func findBrowser() string {
	for _, name := range []string{"chrome", "chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "msedge"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	if runtime.GOOS != "windows" {
		return ""
	}
	paths := []string{
		filepath.Join(os.Getenv("PROGRAMFILES"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("PROGRAMFILES"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("PROGRAMFILES(X86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
	}
	for _, path := range paths {
		if stat, err := os.Stat(path); err == nil && !stat.IsDir() {
			return path
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
