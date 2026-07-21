package registrar

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func (s *Service) loadConfig() error {
	data, err := os.ReadFile(s.configPath)
	if errors.Is(err, os.ErrNotExist) {
		s.config = DefaultConfig()
		return s.saveConfigLocked()
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &s.config); err != nil {
		return fmt.Errorf("读取注册模块配置: %w", err)
	}
	s.config = normalizeConfig(s.config)
	return nil
}

func (s *Service) saveConfigLocked() error {
	s.config.Version = configVersion
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Clean(s.configPath), append(data, '\n'), 0o600)
}

func normalizeConfig(config Config) Config {
	defaults := DefaultConfig()
	legacy := config.Version == 0
	config.Version = configVersion
	config.BrowserPath = strings.TrimSpace(config.BrowserPath)
	config.BrowserMode = strings.ToLower(strings.TrimSpace(config.BrowserMode))
	if config.BrowserMode == "" {
		config.BrowserMode = defaults.BrowserMode
	}
	config.ProxyURL = strings.TrimSpace(config.ProxyURL)
	config.EmailProvider = strings.ToLower(strings.TrimSpace(config.EmailProvider))
	if config.EmailProvider == "" {
		config.EmailProvider = defaults.EmailProvider
	}
	config.DefaultDomains = strings.TrimSpace(config.DefaultDomains)
	config.CloudmailURL = strings.TrimRight(strings.TrimSpace(config.CloudmailURL), "/")
	config.CloudmailAdminEmail = strings.TrimSpace(config.CloudmailAdminEmail)
	config.CloudflareAPIBase = strings.TrimRight(strings.TrimSpace(config.CloudflareAPIBase), "/")
	config.CloudflareAPIKey = strings.TrimSpace(config.CloudflareAPIKey)
	config.CloudflareAuthMode = strings.ToLower(strings.TrimSpace(config.CloudflareAuthMode))
	if config.CloudflareAuthMode == "" {
		config.CloudflareAuthMode = defaults.CloudflareAuthMode
	}
	config.CloudflareDomainsPath = normalizeAPIPath(config.CloudflareDomainsPath, defaults.CloudflareDomainsPath)
	config.CloudflareAccountsPath = normalizeAPIPath(config.CloudflareAccountsPath, defaults.CloudflareAccountsPath)
	config.CloudflareTokenPath = normalizeAPIPath(config.CloudflareTokenPath, defaults.CloudflareTokenPath)
	config.CloudflareMessagesPath = normalizeAPIPath(config.CloudflareMessagesPath, defaults.CloudflareMessagesPath)
	if config.HotmailMaxAliases == 0 {
		config.HotmailMaxAliases = defaults.HotmailMaxAliases
	}
	if config.Count == 0 {
		config.Count = defaults.Count
	}
	if config.Workers == 0 {
		config.Workers = defaults.Workers
	}
	if config.MailTimeoutSeconds == 0 {
		config.MailTimeoutSeconds = defaults.MailTimeoutSeconds
	}
	if config.PageTimeoutSeconds == 0 {
		config.PageTimeoutSeconds = defaults.PageTimeoutSeconds
	}
	if legacy {
		config.PreferProtocolMint = true
	}
	return config
}

func validateConfig(config Config, forStart bool) error {
	if config.Count < 1 || config.Count > 100 {
		return fmt.Errorf("注册数量必须在 1–100 之间")
	}
	if config.Workers < 1 || config.Workers > 3 {
		return fmt.Errorf("注册并发必须在 1–3 之间")
	}
	if config.HotmailMaxAliases < 1 || config.HotmailMaxAliases > 100 {
		return fmt.Errorf("单邮箱别名数必须在 1–100 之间")
	}
	if config.MailTimeoutSeconds < 30 || config.MailTimeoutSeconds > 900 {
		return fmt.Errorf("邮件超时必须在 30–900 秒之间")
	}
	if config.PageTimeoutSeconds < 60 || config.PageTimeoutSeconds > 1200 {
		return fmt.Errorf("页面超时必须在 60–1200 秒之间")
	}
	if config.BrowserMode != "auto" && config.BrowserMode != "headless" && config.BrowserMode != "visible" {
		return fmt.Errorf("浏览器模式必须是 auto、headless 或 visible")
	}
	if !forStart {
		return nil
	}
	switch config.EmailProvider {
	case "hotmail":
		if strings.TrimSpace(config.HotmailAccountsText) == "" {
			return fmt.Errorf("Hotmail 模式需要邮箱----密码----ClientID----refresh_token 四段凭证")
		}
	case "cloudmail":
		if config.CloudmailURL == "" || config.CloudmailAdminEmail == "" || config.CloudmailPassword == "" || config.DefaultDomains == "" {
			return fmt.Errorf("CloudMail 模式需要 URL、管理员邮箱、密码和域名")
		}
	case "cloudflare":
		if config.CloudflareAPIBase == "" {
			return fmt.Errorf("Cloudflare 模式需要 API Base")
		}
		if config.CloudflareAuthMode != "none" && config.CloudflareAuthMode != "bearer" && config.CloudflareAuthMode != "x-api-key" && config.CloudflareAuthMode != "query-key" {
			return fmt.Errorf("Cloudflare 认证方式必须是 none、bearer、x-api-key 或 query-key")
		}
		if config.CloudflareAuthMode != "none" && config.CloudflareAPIKey == "" {
			return fmt.Errorf("Cloudflare 认证方式 %s 需要 API Key", config.CloudflareAuthMode)
		}
	default:
		return fmt.Errorf("当前内置模块只支持 hotmail、cloudmail 和 cloudflare")
	}
	return nil
}

func normalizeAPIPath(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return value
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if runtime.GOOS != "windows" {
			return err
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return err
		}
		return os.Rename(tmpName, path)
	}
	_ = os.Chmod(path, mode)
	return nil
}
