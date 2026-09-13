package webpool

import "time"

// Web 通道号池：用 SSO cookie 调 grok.com 网页版 API
// （/rest/app-chat/conversations/new），对外暴露 OpenAI 兼容的
// /web/v1/chat/completions。与 grokpool（CLI OAuth → cli-chat-proxy）
// 是并列的两条服务通道；网页通道的账号 = 一枚 SSO cookie 组。

const poolVersion = 1

// Account 是一个网页通道账号（SSO cookie 组 + 运行状态）。
// Cookie 内容单独存在 accounts/<id>.json，索引文件只放状态。
type Account struct {
	ID        string    `json:"id"`
	Email     string    `json:"email,omitempty"`
	Source    string    `json:"source,omitempty"` // registrar-cookie | manual
	Disabled  bool      `json:"disabled,omitempty"`
	Class     string    `json:"class,omitempty"` // healthy | rate_limited | blocked | unknown
	ClassNote string    `json:"class_note,omitempty"`
	ClassAt   time.Time `json:"class_at,omitempty"`
	// RateLimitedAt / BlockedAt 触发隔离的时间；rate_limited 到期
	// （webRateLimitTTL）自动恢复参与轮换。
	RateLimitedAt time.Time `json:"rate_limited_at,omitempty"`
	BlockedAt     time.Time `json:"blocked_at,omitempty"`
	SuccessCount  int64     `json:"success_count"`
	FailureCount  int64     `json:"failure_count"`
	LastUsedAt    time.Time `json:"last_used_at,omitempty"`
	LastUsedMs    int64     `json:"last_used_ms,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// Summary 是给 API/UI 的账号统计。
type Summary struct {
	Total       int `json:"total"`
	Available   int `json:"available"`
	Healthy     int `json:"healthy"`
	RateLimited int `json:"rate_limited"`
	Blocked     int `json:"blocked"`
	Disabled    int `json:"disabled"`
}

// Settings 是 Web 通道设置（存于 webpool.json）。
type Settings struct {
	// Enabled 为 false 时 /web/v1 端点返回 503，账号仍可管理。
	Enabled bool `json:"enabled"`
	// ProxyURL 与号池共用一个出口代理：cf_clearance 绑定注册时的出口
	// IP，换出口等于作废。空 = 直连。
	ProxyURL string `json:"proxy_url,omitempty"`
	// CFClearance 是设置级的 grok.com Cloudflare 通行证：绑定出口 IP
	// 与浏览器 UA，约半小时到数小时过期；「采集」或从本机浏览器复制。
	// 账号快照里的 clearance 过期时以此兜底。
	CFClearance string `json:"cf_clearance,omitempty"`
	// ClearanceUserAgent 是采集 clearance 时的浏览器 UA：clearance 与
	// UA 绑定，请求必须用同一 UA 才被 Cloudflare 认可。空 = 内置默认。
	ClearanceUserAgent string `json:"clearance_user_agent,omitempty"`
}

// Status 是 GET /api/web-pool 的负载。
type Status struct {
	Settings    Settings  `json:"settings"`
	LocalAPIKey string    `json:"local_api_key"`
	Accounts    []Account `json:"accounts"`
	Summary     Summary   `json:"summary"`
}

// CookieSet 是一个账号的完整 cookie 组（grok.com / x.ai 域）。
type CookieSet struct {
	SSO         string `json:"sso"`
	SSORW       string `json:"sso_rw"`
	CFClearance string `json:"cf_clearance,omitempty"`
	CFBM        string `json:"cf_bm,omitempty"`
	CUID        string `json:"cuid,omitempty"`
	XAIAnonID   string `json:"xai_anon_id,omitempty"`
}

// accountFile 是 accounts/<id>.json 的磁盘格式。
type accountFile struct {
	Version int       `json:"version"`
	Email   string    `json:"email,omitempty"`
	Source  string    `json:"source,omitempty"`
	Cookies CookieSet `json:"cookies"`
	// RegistrarCookieFile 记录来源快照路径，便于「重新同步」定位。
	RegistrarCookieFile string    `json:"registrar_cookie_file,omitempty"`
	SyncedAt            time.Time `json:"synced_at,omitempty"`
}

// persistedState 是 webpool.json 的磁盘格式。
type persistedState struct {
	Version     int       `json:"version"`
	LocalAPIKey string    `json:"local_api_key"`
	Settings    Settings  `json:"settings"`
	Accounts    []Account `json:"accounts"`
}

// accountAvailable：disabled / blocked 不可用；rate_limited 在
// webRateLimitTTL 后自动恢复。
func accountAvailable(account Account, now time.Time) bool {
	if account.Disabled {
		return false
	}
	switch account.Class {
	case "blocked":
		return false
	case "rate_limited":
		return account.RateLimitedAt.IsZero() || now.Sub(account.RateLimitedAt) > webRateLimitTTL
	default:
		return true
	}
}

func summarizeAccounts(accounts []Account, now time.Time) Summary {
	var summary Summary
	for _, account := range accounts {
		summary.Total++
		if account.Disabled {
			summary.Disabled++
		}
		if accountAvailable(account, now) {
			summary.Available++
		}
		switch account.Class {
		case "healthy":
			summary.Healthy++
		case "rate_limited":
			summary.RateLimited++
		case "blocked":
			summary.Blocked++
		}
	}
	return summary
}
