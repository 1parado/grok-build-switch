package server

import (
	"encoding/base64"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"grok_switch/internal/remoteaccess"
)

const lanSessionCookie = "grok_switch_lan_session"

type loginFailure struct {
	Count   int
	Blocked time.Time
	Window  time.Time
}

type lanAddress struct {
	Address string `json:"address"`
	URL     string `json:"url"`
	PairURL string `json:"pair_url"`
	QRCode  string `json:"qr_code"`
}

func (s *Server) withAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !s.lanAccessEnabled() {
			http.Error(w, "局域网访问未启用", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/pair" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.originAllowed(r) {
			http.Error(w, "请求来源不受信任", http.StatusForbidden)
			return
		}
		cookie, err := r.Cookie(lanSessionCookie)
		authorized := false
		if err == nil && s.RemoteAccess != nil {
			authorized, err = s.RemoteAccess.Authorized(cookie.Value)
		}
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		if !authorized {
			s.writeUnauthorized(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) lanAccessEnabled() bool {
	if s.Settings == nil {
		return false
	}
	current, err := s.Settings.Get()
	return err == nil && current.LANAccessEnabled
}

func (s *Server) originAllowed(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.Host != r.Host {
		return false
	}
	return parsed.Scheme == "http"
}

func (s *Server) writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	if !s.allowLoginAttempt(remoteIP(r)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "登录尝试过于频繁，请稍后再试", http.StatusTooManyRequests)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeError(w, fmt.Errorf("请先使用电脑端生成的二维码完成配对"), http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/pair", http.StatusSeeOther)
}

func (s *Server) allowLoginAttempt(ip string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.loginFails == nil {
		s.loginFails = map[string]loginFailure{}
	}
	now := time.Now()
	entry := s.loginFails[ip]
	if now.Before(entry.Blocked) {
		return false
	}
	if entry.Window.IsZero() || now.Sub(entry.Window) >= time.Minute {
		entry = loginFailure{Window: now}
	}
	entry.Count++
	if entry.Count > 12 {
		entry.Blocked = now.Add(time.Minute)
		s.loginFails[ip] = entry
		return false
	}
	s.loginFails[ip] = entry
	return true
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !s.lanAccessEnabled() {
		writePairPage(w, "局域网访问尚未启用。请先在电脑端设置中开启。", false)
		return
	}
	if !s.allowLoginAttempt(remoteIP(r)) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "配对尝试过于频繁，请稍后再试", http.StatusTooManyRequests)
		return
	}
	if s.RemoteAccess == nil {
		writeError(w, fmt.Errorf("局域网访问凭据未初始化"), http.StatusServiceUnavailable)
		return
	}
	token, ok, err := s.RemoteAccess.ConsumePairing(r.URL.Query().Get("code"))
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		writePairPage(w, "配对码无效或已过期，请回到电脑端重新生成二维码。", false)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     lanSessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   30 * 24 * 60 * 60,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLANAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !isLoopbackRequest(r) {
		writeJSON(w, map[string]any{"enabled": true, "remote": true, "addresses": []lanAddress{}})
		return
	}
	if !s.lanAccessEnabled() {
		writeJSON(w, map[string]any{"enabled": false, "addresses": []lanAddress{}})
		return
	}
	if s.RemoteAccess == nil {
		writeError(w, fmt.Errorf("局域网访问凭据未初始化"), http.StatusServiceUnavailable)
		return
	}
	var snapshot remoteaccess.Snapshot
	var err error
	if r.Method == http.MethodPost {
		snapshot, err = s.RemoteAccess.NewPairing()
	} else {
		snapshot, err = s.RemoteAccess.Get()
		if err == nil && (snapshot.PairingCode == "" || !time.Now().Before(snapshot.PairingExpiry)) {
			snapshot, err = s.RemoteAccess.NewPairing()
		}
	}
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	addresses, err := s.lanAddresses(snapshot.PairingCode)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"enabled":        true,
		"port":           s.ActualPort,
		"addresses":      addresses,
		"pairing_code":   snapshot.PairingCode,
		"pairing_expiry": snapshot.PairingExpiry,
	})
}

func (s *Server) lanAddresses(code string) ([]lanAddress, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	addresses := make([]lanAddress, 0)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			ip = ip.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			address := fmt.Sprintf("%s:%d", ip.String(), s.ActualPort)
			pairURL := fmt.Sprintf("http://%s/pair?code=%s", address, url.QueryEscape(code))
			png, err := qrcode.Encode(pairURL, qrcode.Medium, 256)
			if err != nil {
				return nil, err
			}
			addresses = append(addresses, lanAddress{
				Address: address,
				URL:     fmt.Sprintf("http://%s/", address),
				PairURL: pairURL,
				QRCode:  "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
			})
		}
	}
	return addresses, nil
}

func writePairPage(w http.ResponseWriter, message string, success bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	title := "grok_switch 手机配对"
	if success {
		title = "配对成功"
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>body{margin:0;padding:32px 20px;font:16px/1.6 system-ui,sans-serif;background:#f7f7f8;color:#18181b}main{max-width:480px;margin:12vh auto;padding:28px;background:#fff;border:1px solid #e4e4e7;border-radius:12px;box-shadow:0 8px 24px #0000000a}h1{font-size:22px;margin:0 0 12px}p{color:#52525b}</style><main><h1>%s</h1><p>%s</p></main></html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
