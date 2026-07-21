package cpamint

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
)

const (
	ClientID           = "b1a00492-073a-47ea-816f-4c329264a828"
	Issuer             = "https://auth.x.ai"
	DeviceCodeURL      = "https://auth.x.ai/oauth2/device/code"
	TokenURL           = "https://auth.x.ai/oauth2/token"
	DefaultBaseURL     = "https://cli-chat-proxy.grok.com/v1"
	DefaultRedirectURI = "http://127.0.0.1:56121/callback"
	Scope              = "openid profile email offline_access grok-cli:access api:access"
)

var defaultClientHeaders = map[string]string{
	"x-grok-client-version":      "0.2.93",
	"x-xai-token-auth":           "xai-grok-cli",
	"x-authenticateresponse":     "authenticate-response",
	"x-grok-client-identifier":   "grok-shell",
	"User-Agent":                 "grok-shell/0.2.93 (windows; amd64)",
}

// AuthFile is the CPA-compatible xAI OAuth JSON payload.
type AuthFile struct {
	Type          string            `json:"type"`
	AuthKind      string            `json:"auth_kind"`
	AccessToken   string            `json:"access_token"`
	RefreshToken  string            `json:"refresh_token"`
	TokenType     string            `json:"token_type"`
	ExpiresIn     int               `json:"expires_in"`
	Expired       string            `json:"expired,omitempty"`
	LastRefresh   string            `json:"last_refresh,omitempty"`
	Email         string            `json:"email,omitempty"`
	Sub           string            `json:"sub,omitempty"`
	BaseURL       string            `json:"base_url"`
	TokenEndpoint string            `json:"token_endpoint"`
	RedirectURI   string            `json:"redirect_uri"`
	Disabled      bool              `json:"disabled"`
	Headers       map[string]string `json:"headers,omitempty"`
	IDToken       string            `json:"id_token,omitempty"`
}

func BuildAuthFile(email, accessToken, refreshToken, idToken string, expiresIn int, baseURL string) (AuthFile, error) {
	accessToken = strings.TrimSpace(accessToken)
	refreshToken = strings.TrimSpace(refreshToken)
	if accessToken == "" {
		return AuthFile{}, fmt.Errorf("access_token 不能为空")
	}
	if refreshToken == "" {
		return AuthFile{}, fmt.Errorf("refresh_token 不能为空（CPA/号池需要用它续期）")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(firstNonEmpty(baseURL, DefaultBaseURL)), "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		if strings.HasSuffix(baseURL, "cli-chat-proxy.grok.com") {
			baseURL += "/v1"
		}
	}
	expired, expIn, sub := claimsFromAccessToken(accessToken)
	if expiresIn <= 0 {
		expiresIn = expIn
	}
	if expiresIn <= 0 {
		expiresIn = 21600
	}
	email = strings.TrimSpace(email)
	if email == "" {
		email = jwtStringClaim(accessToken, "email")
	}
	if email == "" && idToken != "" {
		email = jwtStringClaim(idToken, "email")
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	return AuthFile{
		Type:          "xai",
		AuthKind:      "oauth",
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		TokenType:     "Bearer",
		ExpiresIn:     expiresIn,
		Expired:       expired,
		LastRefresh:   now,
		Email:         email,
		Sub:           sub,
		BaseURL:       baseURL,
		TokenEndpoint: TokenURL,
		RedirectURI:   DefaultRedirectURI,
		Disabled:      false,
		Headers:       cloneHeaders(defaultClientHeaders),
		IDToken:       strings.TrimSpace(idToken),
	}, nil
}

func CredentialFileName(email, sub string) string {
	if seg := sanitizeFileSegment(email); seg != "" {
		return "xai-" + seg + ".json"
	}
	if seg := sanitizeFileSegment(sub); seg != "" {
		return "xai-" + seg + ".json"
	}
	return fmt.Sprintf("xai-%d.json", time.Now().UTC().UnixMilli())
}

func WriteAuthFile(authDir string, auth AuthFile) (string, []byte, error) {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return "", nil, fmt.Errorf("认证输出目录不能为空")
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", nil, err
	}
	name := CredentialFileName(auth.Email, auth.Sub)
	dest := filepath.Join(authDir, name)
	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return "", nil, err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(authDir, ".xai-*.tmp")
	if err != nil {
		return "", nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return "", nil, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		if runtime.GOOS == "windows" {
			_ = os.Remove(dest)
			if err = os.Rename(tmpName, dest); err != nil {
				return "", nil, err
			}
		} else {
			return "", nil, err
		}
	}
	_ = os.Chmod(dest, 0o600)
	return dest, data, nil
}

func claimsFromAccessToken(accessToken string) (expired string, expiresIn int, sub string) {
	payload, err := jwtPayload(accessToken)
	if err != nil {
		return "", 0, ""
	}
	exp, _ := payload["exp"].(float64)
	iat, _ := payload["iat"].(float64)
	if exp > 0 {
		expired = time.Unix(int64(exp), 0).UTC().Format("2006-01-02T15:04:05Z")
		if iat > 0 {
			expiresIn = int(exp - iat)
		}
	}
	sub = firstNonEmpty(stringClaim(payload, "sub"), stringClaim(payload, "principal_id"))
	return expired, expiresIn, sub
}

func jwtStringClaim(token, key string) string {
	payload, err := jwtPayload(token)
	if err != nil {
		return ""
	}
	return stringClaim(payload, key)
}

func jwtPayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		padded := parts[1]
		if m := len(padded) % 4; m != 0 {
			padded += strings.Repeat("=", 4-m)
		}
		raw, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			return nil, err
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func stringClaim(payload map[string]any, key string) string {
	v, _ := payload[key].(string)
	return strings.TrimSpace(v)
}

func sanitizeFileSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '@' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
