package webpool

// transport.go — 对 grok.com 的出口传输层。
//
// Cloudflare 有两道指纹门：
//  1. TLS ClientHello（JA3/JA4）——Go 默认 crypto/tls 直接 403；
//     用 uTLS 的 Chrome 模板解决。
//  2. HTTP/2 帧指纹（SETTINGS/头序）——x/net/http2 的实现与浏览器差异
//     明显，实测同样 403；因此把 ALPN 显式收敛为 http/1.1，
//     在 Chrome 指纹的 ClientHello 里以 h1 通信（curl_cffi 同款做法）。
//
// 代理支持：http/https 走 CONNECT 隧道，socks5 直拨；cf_clearance 绑定
// 出口 IP，代理须与注册时一致。

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// webRoundTripper：https 走 Chrome 指纹 h2；http（httptest 等本地目标）
// 走常规 transport。
type webRoundTripper struct {
	h2    *chromeH2Transport
	plain *http.Transport
}

func (w *webRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.EqualFold(req.URL.Scheme, "https") {
		return w.h2.RoundTrip(req)
	}
	return w.plain.RoundTrip(req)
}

// buildWebTransport 构建 Chrome 指纹传输层；proxyURL 为空时直连。
func buildWebTransport(proxyURL string) (*webRoundTripper, error) {
	var proxy *url.URL
	raw := strings.TrimSpace(proxyURL)
	if raw != "" {
		if !strings.Contains(raw, "://") {
			raw = "http://" + raw
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("代理地址无效")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("代理协议只支持 http、https、socks5 或 socks5h")
		}
		proxy = parsed
	}
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}

	dialChrome := func(ctx context.Context, network, addr string) (net.Conn, error) {
		rawConn, err := dialRawConn(ctx, dialer, proxy, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			host = addr
		}
		// Chrome Auto 预设自带 ALPN ["h2","http/1.1"]，server 协商 h2
		// 后由 chromeH2Transport 用 Chrome 帧指纹对话。
		uconn := utls.UClient(rawConn, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
		if err := uconn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("Chrome TLS 指纹握手失败: %w", err)
		}
		return uconn, nil
	}

	h2 := &chromeH2Transport{
		dialTLS: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialChrome(ctx, network, addr)
		},
	}
	plain := &http.Transport{DialContext: dialer.DialContext}
	if proxy != nil {
		plain.Proxy = http.ProxyURL(proxy)
	}
	return &webRoundTripper{h2: h2, plain: plain}, nil
}

// dialRawConn 建立到目标的裸连接：有代理时走 CONNECT 隧道（http/https）。
func dialRawConn(ctx context.Context, dialer *net.Dialer, proxy *url.URL, network, addr string) (net.Conn, error) {
	if proxy == nil {
		return dialer.DialContext(ctx, network, addr)
	}
	scheme := strings.ToLower(proxy.Scheme)
	if scheme == "socks5" || scheme == "socks5h" {
		d := &net.Dialer{Timeout: 20 * time.Second}
		return viaSocks(ctx, d, proxy, network, addr)
	}
	proxyAddr := proxy.Host
	if !strings.Contains(proxyAddr, ":") {
		if strings.EqualFold(scheme, "https") {
			proxyAddr += ":443"
		} else {
			proxyAddr += ":80"
		}
	}
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("连接代理失败: %w", err)
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 失败: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 返回 %d", resp.StatusCode)
	}
	return conn, nil
}

// viaSocks 极简 SOCKS5 拨号（无鉴权 / 用户名密码）。
func viaSocks(ctx context.Context, d *net.Dialer, proxy *url.URL, network, addr string) (net.Conn, error) {
	proxyAddr := proxy.Host
	if !strings.Contains(proxyAddr, ":") {
		proxyAddr += ":1080"
	}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	host, portStr, _ := net.SplitHostPort(addr)
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	ip := net.ParseIP(host)
	req := []byte{0x05, 0x01, 0x00}
	if proxy.User != nil {
		req = []byte{0x05, 0x02, 0x00, 0x02}
	}
	if _, err := conn.Write(req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	buf := make([]byte, 2)
	if _, err := ioReadFull(conn, buf); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if buf[0] != 0x05 {
		_ = conn.Close()
		return nil, fmt.Errorf("SOCKS 版本不符")
	}
	if buf[1] == 0x02 && proxy.User != nil {
		user := proxy.User.Username()
		pass, _ := proxy.User.Password()
		auth := []byte{0x01, byte(len(user))}
		auth = append(auth, user...)
		auth = append(auth, byte(len(pass)))
		auth = append(auth, pass...)
		if _, err := conn.Write(auth); err != nil {
			_ = conn.Close()
			return nil, err
		}
		ack := make([]byte, 2)
		if _, err := ioReadFull(conn, ack); err != nil || ack[1] != 0x00 {
			_ = conn.Close()
			return nil, fmt.Errorf("SOCKS 鉴权失败")
		}
	}
	// CONNECT 命令（域名或 IP）。
	var req2 []byte
	if ip == nil {
		req2 = append(req2, 0x05, 0x01, 0x00, 0x03, byte(len(host)))
		req2 = append(req2, host...)
	} else {
		if ip.To4() != nil {
			req2 = append(req2, 0x05, 0x01, 0x00, 0x01)
		} else {
			req2 = append(req2, 0x05, 0x01, 0x00, 0x04)
		}
	}
	req2 = append(req2, byte(port>>8), byte(port&0xff))
	if _, err := conn.Write(req2); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reply := make([]byte, 4)
	if _, err := ioReadFull(conn, reply); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if reply[1] != 0x00 {
		_ = conn.Close()
		return nil, fmt.Errorf("SOCKS CONNECT 被拒: 0x%02x", reply[1])
	}
	// 跳过绑定地址。
	switch reply[3] {
	case 0x01:
		_, err = ioReadFull(conn, make([]byte, 4+2))
	case 0x03:
		l := make([]byte, 1)
		if _, err = ioReadFull(conn, l); err == nil {
			_, err = ioReadFull(conn, make([]byte, int(l[0])+2))
		}
	case 0x04:
		_, err = ioReadFull(conn, make([]byte, 16+2))
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func ioReadFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
