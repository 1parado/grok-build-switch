package server

// webpool.go — Web 通道号池（grok2api 式）：
//   /web/v1/models、/web/v1/chat/completions  OpenAI 兼容对外端点（思考 → reasoning_content）
//   /api/web-pool*                             管理端点（状态/设置/导入/账号）
//
// 与 /grok/v1（CLI OAuth → cli-chat-proxy）并列的第二条服务通道，
// 账号 = SSO cookie，上游 = grok.com 网页 API。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"grok_switch/internal/profiles"
	"grok_switch/internal/registrar"
	"grok_switch/internal/webpool"
)

func (s *Server) webPoolReady() bool {
	return s.WebPool != nil
}

// handleWebV1 是 /web/v1 的总入口：按后缀分发 models / chat/completions。
func (s *Server) handleWebV1(w http.ResponseWriter, r *http.Request) {
	if !s.webPoolReady() {
		writeError(w, fmt.Errorf("Web 通道未初始化"), http.StatusServiceUnavailable)
		return
	}
	if !s.WebPool.Status().Settings.Enabled {
		writeError(w, fmt.Errorf("Web 通道未启用（设置 → 号池 → Web 通道）"), http.StatusServiceUnavailable)
		return
	}
	if !s.WebPool.Authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, fmt.Errorf("无效的 Web 通道 API Key"), http.StatusUnauthorized)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, "/web/v1")
	switch {
	case suffix == "/models" && r.Method == http.MethodGet:
		s.handleWebV1Models(w)
	case suffix == "/chat/completions" && r.Method == http.MethodPost:
		s.handleWebV1ChatCompletions(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleWebV1Models(w http.ResponseWriter) {
	writeJSONStatus(w, map[string]any{"object": "list", "data": webpool.ModelList()}, http.StatusOK)
}

type webChatRequest struct {
	Model    string                  `json:"model"`
	Stream   bool                    `json:"stream"`
	Messages []webpool.OpenAIMessage `json:"messages"`
}

func (s *Server) handleWebV1ChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req webChatRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, fmt.Errorf("请求体不是合法 JSON: %w", err), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, fmt.Errorf("messages 不能为空"), http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		req.Model = webpool.DefaultModel
	}
	transcript := webpool.FlattenTranscript(req.Messages)
	if transcript == "" {
		writeError(w, fmt.Errorf("messages 内容为空"), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	id := fmt.Sprintf("chatweb-%d", time.Now().UnixNano())
	opts := webpool.ChatOptions{Model: req.Model, Message: transcript}

	if req.Stream {
		s.streamWebChat(w, ctx, id, req.Model, opts)
		return
	}

	var think, text strings.Builder
	_, err = s.WebPool.Chat(ctx, opts, func(ev webpool.WebStreamEvent) error {
		if ev.ThinkDelta != "" {
			think.WriteString(ev.ThinkDelta)
		} else {
			text.WriteString(ev.TextDelta)
		}
		return nil
	})
	if err != nil {
		writeError(w, fmt.Errorf("Web 通道请求失败: %v", err), http.StatusBadGateway)
		return
	}
	writeJSONStatus(w, webpool.NewCompletion(id, req.Model, text.String(), think.String(), len(transcript)), http.StatusOK)
}

func (s *Server) streamWebChat(w http.ResponseWriter, ctx context.Context, id, model string, opts webpool.ChatOptions) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	writeFrame := func(frame any) bool {
		data, err := json.Marshal(frame)
		if err != nil {
			return false
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	if !writeFrame(webpool.NewChunk(id, model, "", "")) {
		return
	}
	_, err := s.WebPool.Chat(ctx, opts, func(ev webpool.WebStreamEvent) error {
		if ev.ThinkDelta != "" {
			if !writeFrame(webpool.NewChunk(id, model, "", ev.ThinkDelta)) {
				return fmt.Errorf("客户端断开")
			}
			return nil
		}
		if ev.TextDelta != "" {
			if !writeFrame(webpool.NewChunk(id, model, ev.TextDelta, "")) {
				return fmt.Errorf("客户端断开")
			}
		}
		return nil
	})
	if err != nil {
		// 流已开：用 OpenAI 错误帧收尾，而不是换 HTTP 状态。
		_ = writeFrame(map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
	}
	_ = writeFrame(webpool.NewFinishChunk(id, model))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// ---------- 管理端点 ----------

func (s *Server) handleWebPool(w http.ResponseWriter, r *http.Request) {
	if !s.webPoolReady() {
		writeError(w, fmt.Errorf("Web 通道未初始化"), http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		status := s.WebPool.Status()
		status.LocalAPIKey = "" // key 只在专门的 reveal 端点返回
		writeJSONStatus(w, status, http.StatusOK)
	case http.MethodPut:
		var settings struct {
			Enabled     bool   `json:"enabled"`
			ProxyURL    string `json:"proxy_url"`
			CFClearance string `json:"cf_clearance"`
		}
		if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		status, err := s.WebPool.UpdateSettings(webpool.Settings{Enabled: settings.Enabled, ProxyURL: settings.ProxyURL, CFClearance: settings.CFClearance})
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		status.LocalAPIKey = ""
		writeJSONStatus(w, status, http.StatusOK)
	default:
		methodNotAllowed(w)
	}
}

// handleWebPoolImport 处理两类导入：
//
//	POST /api/web-pool/sync   —— 扫描注册机 cookie 快照目录
//	POST /api/web-pool/manual —— 粘贴 SSO（body: {"sso": "..."}，支持多行/逗号分隔）
func (s *Server) handleWebPoolImport(w http.ResponseWriter, r *http.Request) {
	if !s.webPoolReady() {
		writeError(w, fmt.Errorf("Web 通道未初始化"), http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	switch strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/web-pool/"), "/") {
	case "sync":
		cookieDir := s.RegistrarCookieDir()
		if cookieDir == "" {
			writeError(w, fmt.Errorf("注册机模块不可用，无法定位 cookie 目录"), http.StatusBadRequest)
			return
		}
		added, refreshed, total, err := s.WebPool.SyncRegistrarCookies(cookieDir)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSONStatus(w, map[string]any{
			"added": added, "refreshed": refreshed, "total": total,
			"message": fmt.Sprintf("导入完成：新增 %d，刷新 %d，共 %d", added, refreshed, total),
		}, http.StatusOK)
	case "manual":
		var req struct {
			SSO string `json:"sso"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.SSO) == "" {
			writeError(w, fmt.Errorf("sso 不能为空"), http.StatusBadRequest)
			return
		}
		imported, status, err := s.WebPool.ImportManual(req.SSO)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		status.LocalAPIKey = ""
		writeJSONStatus(w, map[string]any{"imported": imported, "status": status}, http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

// handleWebPoolAccount 处理 PATCH（启停）/ DELETE（删除）。
func (s *Server) handleWebPoolAccount(w http.ResponseWriter, r *http.Request) {
	if !s.webPoolReady() {
		writeError(w, fmt.Errorf("Web 通道未初始化"), http.StatusServiceUnavailable)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/web-pool/accounts/"), "/")
	if id == "" {
		writeError(w, fmt.Errorf("缺少账号 id"), http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Disabled *bool `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if req.Disabled == nil {
			writeError(w, fmt.Errorf("缺少 disabled 字段"), http.StatusBadRequest)
			return
		}
		status, err := s.WebPool.SetDisabled(id, *req.Disabled)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		status.LocalAPIKey = ""
		writeJSONStatus(w, status, http.StatusOK)
	case http.MethodDelete:
		status, err := s.WebPool.Delete(id)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		status.LocalAPIKey = ""
		writeJSONStatus(w, status, http.StatusOK)
	default:
		methodNotAllowed(w)
	}
}

// handleWebPoolKey 返回 Web 通道 Base URL 与 API Key（管理界面用）。
func (s *Server) handleWebPoolKey(w http.ResponseWriter, r *http.Request) {
	if !s.webPoolReady() {
		writeError(w, fmt.Errorf("Web 通道未初始化"), http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	base := fmt.Sprintf("http://127.0.0.1:%d/web/v1", s.ActualPort)
	writeJSONStatus(w, map[string]any{
		"base_url": base,
		"api_key":  s.WebPool.LocalAPIKey(),
	}, http.StatusOK)
}

// upsertWebChannelProfile 把 Web 通道注册为内置工作台可用的一条供应商
// Profile（openai_chat 格式指向 loopback /web/v1）。native 引擎的
// ChatCompletionsProvider 原生解析 reasoning_content，思考直接显示。
func (s *Server) upsertWebChannelProfile() (profiles.Profile, error) {
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/web/v1", s.ActualPort)
	apiKey := s.WebPool.LocalAPIKey()
	models := []profiles.ModelDef{}
	for _, m := range webpool.ModelList() {
		name, _ := m["id"].(string)
		models = append(models, profiles.ModelDef{
			Name:                    name,
			DisplayName:             name,
			Model:                   name,
			BaseURL:                 baseURL,
			APIKey:                  apiKey,
			SupportsReasoningEffort: strings.Contains(name, "reasoning"),
			ReasoningEfforts:        []string{"low", "medium", "high"},
			ContextWindow:           131072,
		})
	}
	list, err := s.Profiles.List()
	if err != nil {
		return profiles.Profile{}, err
	}
	var existing *profiles.Profile
	for i := range list {
		if list[i].Name == webChannelProfileName {
			existing = &list[i]
			break
		}
	}
	profile := profiles.Profile{
		Name:            webChannelProfileName,
		Template:        "openai_chat",
		UpstreamFormat:  "openai_chat",
		BaseURL:         baseURL,
		APIKey:          apiKey,
		AvailableModels: modelNames(models),
		DefaultModel:    webpool.DefaultModel,
		WebSearchModel:  webpool.DefaultModel,
		Models:          models,
	}
	if existing == nil {
		return s.Profiles.Create(profile)
	}
	profile.ID = existing.ID
	profile.DefaultModel = firstNonEmptyServer(existing.DefaultModel, profile.DefaultModel)
	profile.WebSearchModel = firstNonEmptyServer(existing.WebSearchModel, profile.WebSearchModel)
	updated, err := s.Profiles.Update(profile.ID, profile)
	if err != nil {
		return profiles.Profile{}, err
	}
	return updated, nil
}

const webChannelProfileName = "Web 通道（思考）"

// handleWebPoolConnect 一键把 Web 通道接入内置工作台（upsert Profile）。
func (s *Server) handleWebPoolConnect(w http.ResponseWriter, r *http.Request) {
	if !s.webPoolReady() {
		writeError(w, fmt.Errorf("Web 通道未初始化"), http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	profile, err := s.upsertWebChannelProfile()
	if err != nil {
		writeError(w, fmt.Errorf("创建 Web 通道 Profile 失败: %w", err), http.StatusInternalServerError)
		return
	}
	s.changed()
	writeJSONStatus(w, map[string]any{
		"message":    "已接入内置工作台，Profile：" + profile.Name,
		"profile_id": profile.ID,
		"base_url":   profile.BaseURL,
	}, http.StatusOK)
}

// handleWebPoolClearance 启动可见浏览器采集 grok.com 的 Cloudflare
// 通行证并写入设置（采集期间浏览器对用户可见，必要时可手动点验证码）。
func (s *Server) handleWebPoolClearance(w http.ResponseWriter, r *http.Request) {
	if !s.webPoolReady() {
		writeError(w, fmt.Errorf("Web 通道未初始化"), http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Registrar == nil {
		writeError(w, fmt.Errorf("注册机模块不可用"), http.StatusServiceUnavailable)
		return
	}
	config := s.Registrar.Get().Config
	go func() {
		logf := func(line string) { fmt.Fprintf(os.Stderr, "[webpool-clearance] %s\n", line) }
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		clearance, err := registrar.HarvestGrokComClearance(ctx, config, 4*time.Minute, logf)
		if err != nil {
			logf("采集失败: " + err.Error())
			return
		}
		value, userAgent, hasSep := strings.Cut(clearance, "\x00")
		if !hasSep || strings.TrimSpace(value) == "" {
			value = clearance
			userAgent = ""
		}
		status := s.WebPool.Status()
		status.Settings.CFClearance = value
		if strings.TrimSpace(userAgent) != "" {
			status.Settings.ClearanceUserAgent = strings.TrimSpace(userAgent)
		}
		if _, updateErr := s.WebPool.UpdateSettings(status.Settings); updateErr != nil {
			logf("写入设置失败: " + updateErr.Error())
			return
		}
		logf("cf_clearance 已更新到 Web 通道设置")
	}()
	writeJSONStatus(w, map[string]any{"message": "浏览器已启动，正在采集 Cloudflare 通行证（保持浏览器可见，必要时可手动完成验证）"}, http.StatusAccepted)
}

// RegistrarCookieDir 暴露注册机 cookie 目录（nil 安全）。
func (s *Server) RegistrarCookieDir() string {
	if s.Registrar == nil {
		return ""
	}
	return s.Registrar.CookieDir()
}
