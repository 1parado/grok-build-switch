package llm

import (
	"net/http"
	"strings"
	"time"
)

// UpstreamTarget 描述一次 Generate 的上游连接参数，由 server 层从当前
// 生效 Profile 解析得到；Provider 不直接依赖 profiles 包，保持引擎与
// 存储层解耦（设计文档 §5 分层契约）。
type UpstreamTarget struct {
	// BaseURL 是上游根地址（loopback 代理或第三方中转），不带尾斜杠。
	BaseURL string
	// APIKey 是本地代理 key 或上游 key。
	APIKey string
	// ExtraHeaders 附加到每个请求（如 UA 覆写）。
	ExtraHeaders map[string]string
	// Timeout 单请求超时；0 用适配器默认。
	Timeout time.Duration
	// ProxyURL 可选 HTTP/SOCKS5 代理；空直连。
	ProxyURL string
}

// NewAPIError 构造归一化上游错误。retryAfter 仅在限流等场景非零。
func NewAPIError(kind string, status int, upstream string, retryAfter time.Duration) *APIError {
	return &APIError{Kind: kind, StatusCode: status, Upstream: truncateUpstream(upstream), RetryAfter: retryAfter}
}

// ClassifyStatus 把 HTTP 状态码归类为 APIError.Kind。
func ClassifyStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return "timeout"
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "auth"
	case status == http.StatusRequestEntityTooLarge:
		return "too_large"
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusInsufficientStorage:
		return "overloaded"
	case status >= 400 && status < 500:
		return "invalid_request"
	case status >= 500:
		return "overloaded"
	default:
		return "unknown"
	}
}

// RetryableKind 判断错误类别是否值得指数退避重试（代理层 failover 之外的引擎侧重试）。
func RetryableKind(kind string) bool {
	switch kind {
	case "rate_limited", "timeout", "overloaded", "network":
		return true
	default:
		return false
	}
}

func truncateUpstream(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}
