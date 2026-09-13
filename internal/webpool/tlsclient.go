package webpool

// tlsclient.go — 用 bogdanfinn/tls-client 的 Chrome_152 指纹替代
// 自定义 uTLS + h2 transport。
//
// tls-client 内建 Chrome 的完整指纹（TLS ClientHello + HTTP/2 帧序列 +
// HPACK 头序），是 Python curl_cffi 的 Go 等价物。这里做 net/http ↔
// fhttp 的薄适配，让 Manager.Do() 的既有调用（chatOnce / statsig）
// 零改动换底层。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// newChromeClient 构造 Chrome_152 指纹的 tls-client（可带代理）。
func newChromeClient(proxyURL string) (tls_client.HttpClient, error) {
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(120),
		tls_client.WithClientProfile(profiles.Chrome_152),
		tls_client.WithNotFollowRedirects(),
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" {
		options = append(options, tls_client.WithProxyUrl(proxyURL))
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		return nil, fmt.Errorf("构造 Chrome 152 客户端: %w", err)
	}
	return client, nil
}

// tlsRoundTripper 把 tls-client 适配为 net/http 的 RoundTripper。
type tlsRoundTripper struct {
	client tls_client.HttpClient
}

func (t *tlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// 读取 net/http 请求体，转为 fhttp 请求。
	var bodyReader io.Reader
	if req.Body != nil {
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		if len(bodyBytes) > 0 {
			bodyReader = bytes.NewReader(bodyBytes)
		}
	}
	freq, err := fhttp.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	for key, values := range req.Header {
		for _, v := range values {
			freq.Header.Add(key, v)
		}
	}
	fresp, err := t.client.Do(freq)
	if err != nil {
		return nil, err
	}
	// fhttp.Response → net/http.Response（body 直接复用，其余浅拷贝）。
	resp := &http.Response{
		Status:        fresp.Status,
		StatusCode:    fresp.StatusCode,
		Proto:         fresp.Proto,
		ProtoMajor:    fresp.ProtoMajor,
		ProtoMinor:    fresp.ProtoMinor,
		Header:        http.Header{},
		Body:          fresp.Body,
		ContentLength: fresp.ContentLength,
		Request:       req,
	}
	for key, values := range fresp.Header {
		for _, v := range values {
			resp.Header.Add(key, v)
		}
	}
	return resp, nil
}
