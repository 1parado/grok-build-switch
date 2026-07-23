package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"grok_switch/internal/profiles"
)

const imagineProxyPrefix = "/imagine/v1"

func (s *Server) localImagineURL() string {
	if s.ActualPort <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.ActualPort, imagineProxyPrefix)
}

func (s *Server) handleImagineProxy(w http.ResponseWriter, r *http.Request) {
	image, err := s.activeImageGeneration()
	if err != nil {
		writeError(w, err, http.StatusServiceUnavailable)
		return
	}
	upstreamRaw := strings.TrimRight(strings.TrimSpace(image.BaseURL), "/")
	if upstreamRaw == "" {
		writeError(w, fmt.Errorf("当前供应商未配置生图服务地址"), http.StatusServiceUnavailable)
		return
	}
	upstream, err := url.Parse(upstreamRaw)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		writeError(w, fmt.Errorf("生图服务地址无效: %s", image.BaseURL), http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(image.APIKey) == "" {
		writeError(w, fmt.Errorf("当前供应商未配置生图 API Key"), http.StatusServiceUnavailable)
		return
	}

	// Optional body rewrite: map catalog aliases to the configured upstream model id.
	body, contentType, err := rewriteImagineRequestBody(r, image.Model)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}

	proxyRequest := r.Clone(r.Context())
	if body != nil {
		proxyRequest.Body = io.NopCloser(bytes.NewReader(body))
		proxyRequest.ContentLength = int64(len(body))
		proxyRequest.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		if contentType != "" {
			proxyRequest.Header.Set("Content-Type", contentType)
		}
	}

	suffix := strings.TrimPrefix(r.URL.Path, imagineProxyPrefix)
	if suffix == "" {
		suffix = "/"
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}

	proxy := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: upstream.Scheme,
		Host:   upstream.Host,
	})
	originalDirector := proxy.Director
	proxy.Director = func(out *http.Request) {
		originalDirector(out)
		out.URL.Scheme = upstream.Scheme
		out.URL.Host = upstream.Host
		basePath := strings.TrimSuffix(upstream.Path, "/")
		out.URL.Path = basePath + suffix
		out.URL.RawPath = ""
		out.URL.RawQuery = r.URL.RawQuery
		out.Host = upstream.Host
		out.Header.Del("x-api-key")
		out.Header.Set("Authorization", "Bearer "+image.APIKey)
		out.Header.Set("x-api-key", image.APIKey)
	}
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, proxyErr error) {
		if !errors.Is(proxyErr, context.Canceled) {
			writeError(writer, fmt.Errorf("生图上游请求失败: %w", proxyErr), http.StatusBadGateway)
		}
	}
	proxy.ServeHTTP(w, proxyRequest)
}

func (s *Server) activeImageGeneration() (*profiles.ImageGenerationConfig, error) {
	if s.Profiles == nil {
		return nil, fmt.Errorf("供应商存储不可用")
	}
	list, err := s.Profiles.List()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if !list[i].IsActive {
			continue
		}
		image := profiles.Normalize(list[i]).ImageGeneration
		if image == nil || !image.Enabled {
			return nil, fmt.Errorf("当前供应商未启用生图")
		}
		return image, nil
	}
	return nil, fmt.Errorf("没有已启用的供应商")
}

func rewriteImagineRequestBody(r *http.Request, upstreamModel string) ([]byte, string, error) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if r.Body == nil || upstreamModel == "" {
		return nil, "", nil
	}
	contentType := r.Header.Get("Content-Type")
	// Leave multipart image edits and other non-JSON payloads untouched.
	if contentType != "" && !strings.Contains(strings.ToLower(contentType), "json") {
		return nil, "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	_ = r.Body.Close()
	if err != nil {
		return nil, "", err
	}
	// Restore body for the reverse proxy when we do not rewrite.
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	if len(raw) == 0 {
		return nil, contentType, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		// Non-JSON body: pass through unchanged.
		return raw, contentType, nil
	}
	model, _ := payload["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" || model == upstreamModel || !isImagineCatalogModel(model) {
		return raw, contentType, nil
	}
	payload["model"] = upstreamModel
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	return rewritten, contentType, nil
}

func isImagineCatalogModel(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "grok-imagine-image", "grok-imagine-image-quality", "grok-imagine-image-lite", "grok-imagine-video":
		return true
	default:
		return false
	}
}
