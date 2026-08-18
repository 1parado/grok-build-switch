package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"grok_switch/internal/grokauth"
	"grok_switch/internal/grokpool"
	"grok_switch/internal/profiles"
)

const grokAuthProfileName = "Grok Auth（本地代理）"

// grok-4.5 已被上游下线（/models 只返回 grok-4.6），存量 Profile 仍钉在
// 旧默认值上时由 migrateGrokAuthModel* 自动升级到 grok-4.6。
const (
	currentGrokAuthChatModel = "grok-4.6"
	retiredGrokAuthChatModel = "grok-4.5"
)

var defaultGrokAuthModels = []profiles.ModelDef{
	{
		Name:                  currentGrokAuthChatModel,
		Model:                 currentGrokAuthChatModel,
		APIBackend:            "responses",
		SupportsBackendSearch: true,
		ContextWindow:         500000,
		MaxCompletionTokens:   65536,
	},
	{
		Name:                  "grok-composer-2.5-fast",
		Model:                 "grok-composer-2.5-fast",
		APIBackend:            "responses",
		SupportsBackendSearch: true,
		ContextWindow:         200000,
		MaxCompletionTokens:   32768,
	},
}

type grokAuthResponse struct {
	Configured       bool              `json:"configured"`
	SingleConfigured bool              `json:"single_configured"`
	PoolAccounts     int               `json:"pool_accounts"`
	Email            string            `json:"email,omitempty"`
	ExpiresAt        *time.Time        `json:"expires_at,omitempty"`
	NeedsRefresh     bool              `json:"needs_refresh"`
	LocalAPIKey      string            `json:"local_api_key,omitempty"`
	Source           string            `json:"source,omitempty"`
	ImportedAt       *time.Time        `json:"imported_at,omitempty"`
	LastRefresh      *time.Time        `json:"last_refresh,omitempty"`
	BaseURL          string            `json:"base_url,omitempty"`
	Profile          *profiles.Profile `json:"profile,omitempty"`
	Warning          string            `json:"warning,omitempty"`
}

func (s *Server) handleGrokAuth(w http.ResponseWriter, r *http.Request) {
	if s.GrokAuth == nil {
		writeError(w, fmt.Errorf("Grok auth store 未初始化"), http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		response, err := s.grokAuthStatusResponse()
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, response)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, fmt.Errorf("读取认证 JSON: %w", err), http.StatusBadRequest)
			return
		}
		if s.GrokPool != nil {
			if _, err := s.GrokPool.Import([]grokpool.ImportFile{{Name: "grok-auth-import.json", Content: string(raw)}}); err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
		}
		warnings := make([]string, 0)
		if s.GrokPool != nil {
			_ = s.GrokAuth.SetProxyURL(s.GrokPool.Status().Settings.ProxyURL)
		}
		if _, err := s.GrokAuth.Import(raw); err != nil {
			warnings = append(warnings, "统一号池已导入，但兼容单账号副本写入失败: "+err.Error())
		} else if _, err := s.GrokAuth.Token(r.Context()); err != nil {
			warnings = append(warnings, "凭据已导入号池，但兼容单账号 token 刷新失败: "+err.Error())
		}
		profile, err := s.upsertGrokAuthProfile()
		if err != nil {
			writeError(w, fmt.Errorf("凭据已导入，但创建本地 profile 失败: %w", err), http.StatusInternalServerError)
			return
		}
		response, err := s.grokAuthStatusResponse()
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		response.Profile = &profile
		response.Warning = strings.Join(warnings, "；")
		s.changed()
		writeJSONStatus(w, response, http.StatusCreated)
	case http.MethodDelete:
		if err := s.GrokAuth.Delete(); err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		var syncErr error
		if s.poolConfigured() {
			_, syncErr = s.upsertGrokAuthProfile()
		} else {
			syncErr = s.removeGrokAuthProfile()
		}
		if syncErr != nil {
			writeError(w, syncErr, http.StatusInternalServerError)
			return
		}
		s.changed()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleGrokAuthRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.GrokAuth == nil {
		writeError(w, fmt.Errorf("Grok auth store 未初始化"), http.StatusServiceUnavailable)
		return
	}
	if _, err := s.GrokAuth.Refresh(r.Context()); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, err, status)
		return
	}
	response, err := s.grokAuthStatusResponse()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, response)
}

func (s *Server) handleGrokProxy(w http.ResponseWriter, r *http.Request) {
	var token string
	var poolAccountID string
	var err error
	authorized := false
	if s.GrokPool != nil && s.GrokPool.Authorized(r) {
		sessionID := firstNonEmptyServer(r.Header.Get("x-grok-conv-id"), r.Header.Get("x-session-id"))
		token, poolAccountID, err = s.GrokPool.NextToken(r.Context(), sessionID)
		if err != nil && s.singleGrokAuthConfigured() {
			poolAccountID = ""
			token, err = s.GrokAuth.Token(r.Context())
		}
		authorized = true
	} else if s.GrokAuth != nil && s.GrokAuth.Authorized(r) {
		token, err = s.GrokAuth.Token(r.Context())
		authorized = true
	}
	if !authorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="grok_switch"`)
		writeError(w, fmt.Errorf("无效的本地 API Key"), http.StatusUnauthorized)
		return
	}
	if err != nil {
		writeError(w, err, http.StatusBadGateway)
		return
	}
	// 内置 image_gen / image_edit 工具走账号池，绕开官方额度限制。
	// 生成（images/generations）由账号池引擎接管；编辑（images/edits）
	// 需要参考图且当前引擎未实现编辑协议，返回明确错误引导模型改用
	// 重新生成，避免 403 让模型困惑。
	if s.Imagine != nil && s.Imagine.AccountCount() > 0 {
		if isImageGenProxyPath(r.URL.Path) {
			if handled, engineErr := s.handleImageGenViaEngine(w, r); handled {
				if engineErr != nil {
					writeError(w, engineErr, http.StatusBadGateway)
				}
				return
			}
		} else if isImageEditProxyPath(r.URL.Path) {
			s.handleImageEditUnavailable(w)
			return
		} else if isChatCompletionsProxyPath(r.URL.Path) {
			// OpenAI 兼容 Harness 场景：注入 generate_image 工具声明，
			// 模型发起生图调用时代为执行并返回最终结果。
			if handled, err := s.handleChatCompletionsWithImageGen(w, r, token); handled {
				if err != nil {
					writeError(w, err, http.StatusBadGateway)
				}
				return
			}
		}
	}
	target, err := url.Parse(grokauth.UpstreamURL())
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}

	proxyRequest := r.Clone(r.Context())
	suffix := strings.TrimPrefix(r.URL.Path, "/grok/v1")
	if suffix == "" {
		suffix = "/"
	}
	proxyRequest.URL.Path = suffix
	proxyRequest.URL.RawPath = ""

	proxy := httputil.NewSingleHostReverseProxy(target)
	if s.GrokPool != nil {
		if transport := s.GrokPool.Transport(); transport != nil {
			proxy.Transport = transport
		}
	}
	originalDirector := proxy.Director
	proxy.Director = func(out *http.Request) {
		originalDirector(out)
		out.Host = target.Host
		out.Header.Del("x-api-key")
		out.Header.Set("Authorization", "Bearer "+token)
		out.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
		out.Header.Set("x-grok-client-version", "0.2.93")
		out.Header.Set("User-Agent", "xai-grok-workspace/0.2.93")
	}
	proxy.FlushInterval = -1
	proxy.ModifyResponse = func(response *http.Response) error {
		if s.GrokPool == nil || poolAccountID == "" || response.StatusCode < 400 {
			return nil
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if readErr != nil {
			return nil
		}
		response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), response.Body))
		s.GrokPool.ObserveResponse(poolAccountID, response.StatusCode, string(raw))
		return nil
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, proxyErr error) {
		if !errors.Is(proxyErr, context.Canceled) {
			writeError(writer, fmt.Errorf("Grok 上游请求失败: %w", proxyErr), http.StatusBadGateway)
		}
	}
	proxy.ServeHTTP(w, proxyRequest)
}

// isImageGenProxyPath 判断是否为内置 image_gen 工具的图片生成请求
// （Grok CLI 以 OpenAI Images API 格式 POST /v1/images/generations）。
func isImageGenProxyPath(requestPath string) bool {
	suffix := strings.TrimPrefix(requestPath, "/grok/v1")
	return strings.HasSuffix(suffix, "/images/generations")
}

// isImageEditProxyPath 判断是否为内置 image_edit 工具的图片编辑请求。
func isImageEditProxyPath(requestPath string) bool {
	suffix := strings.TrimPrefix(requestPath, "/grok/v1")
	return strings.HasSuffix(suffix, "/images/edits")
}

// handleImageEditUnavailable 在账号池引擎不支持图片编辑协议时，返回可读的
// 错误信息，引导模型改用 generate_image 重新生成，而不是撞官方额度 403。
func (s *Server) handleImageEditUnavailable(w http.ResponseWriter) {
	writeJSONStatus(w, map[string]string{
		"code":  "image_edit_unavailable",
		"error": "图片编辑（image_edit）当前不可用：本地账号池仅支持全新生成，不支持基于参考图的修改。请不要重试 image_edit，也不要查找、读取或传递任何参考图文件（不需要参考图）。请直接调用 generate_image 工具，撰写包含所需风格、氛围、构图等完整细节的新提示词重新生成图片；如需保留原图主体或构图，把这些描述写进新提示词即可。",
	}, http.StatusUnprocessableEntity)
}

// handleImageGenViaEngine 用本地账号池（ImagineEngine）执行一次图片生成，
// 并按 OpenAI Images API 格式返回，使内置 image_gen 工具无需官方额度。
// 返回 handled=false 时表示未接管（视频模型或请求不完整），调用方应继续
// 走官方代理；此时请求体已还原。
func (s *Server) handleImageGenViaEngine(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Method != http.MethodPost || s.Imagine == nil {
		return false, nil
	}
	var req struct {
		Model       string `json:"model"`
		Prompt      string `json:"prompt"`
		N           int    `json:"n"`
		Size        string `json:"size"`
		AspectRatio string `json:"aspect_ratio"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	_ = r.Body.Close()
	if err != nil {
		return false, nil
	}
	// 非 JSON（如 multipart 图片编辑）不接管，还原后转发官方。
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") {
		restoreRequestBody(r, raw)
		return false, nil
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		restoreRequestBody(r, raw)
		return false, nil
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "grok-imagine-image"
	}
	// 视频生成仍走官方（当前账号池引擎仅支持静态图）。
	if strings.Contains(strings.ToLower(model), "video") {
		restoreRequestBody(r, raw)
		return false, nil
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return true, fmt.Errorf("prompt 不能为空")
	}
	aspect := strings.TrimSpace(req.AspectRatio)
	if aspect == "" {
		aspect = req.Size
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	res := s.Imagine.Generate(ctx, prompt, model, imageSizeToAspectRatio(aspect))
	if !res.OK {
		return true, fmt.Errorf("%s: %s", res.ErrCode, res.ErrMsg)
	}
	if len(res.Images) == 0 {
		return true, fmt.Errorf("未收到图片数据")
	}
	imageURL := res.Images[0]
	data, readErr := os.ReadFile(filepath.Join(s.Imagine.outputsDir, path.Base(imageURL)))
	if readErr != nil {
		return true, fmt.Errorf("读取生成的图片失败: %w", readErr)
	}
	// 额外复制一份到系统下载目录，方便用户直接使用。
	savedPath := ""
	if dlPath, dlErr := saveImageToDownloads(filepath.Join(s.Imagine.outputsDir, path.Base(imageURL))); dlErr == nil {
		savedPath = dlPath
	} else {
		fmt.Fprintf(os.Stderr, "grok_switch: save image to downloads: %v\n", dlErr)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", s.ActualPort)
	writeJSON(w, map[string]any{
		"created": time.Now().Unix(),
		"data": []map[string]any{{
			"url":      baseURL + imageURL,
			"b64_json": base64.StdEncoding.EncodeToString(data),
			"model":    res.ModelName,
		}},
		"saved_to": savedPath,
	})
	return true, nil
}

// saveImageToDownloads 把生成图片复制一份到系统默认下载目录
// （~/Downloads，Windows 为 %USERPROFILE%\Downloads），文件名保持唯一。
func saveImageToDownloads(srcPath string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	downloadDir := filepath.Join(home, "Downloads")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(downloadDir, filepath.Base(srcPath))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

func restoreRequestBody(r *http.Request, raw []byte) {
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(raw)))
}

// imageSizeToAspectRatio 把 OpenAI Images API 的 size 参数（比例字符串或
// 像素尺寸）映射到引擎的 aspect ratio；不认识的尺寸回退 1:1。
func imageSizeToAspectRatio(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "16:9", "1792x1024", "1920x1080":
		return "16:9"
	case "9:16", "1024x1792", "1080x1920":
		return "9:16"
	case "4:3", "1280x960", "1152x896", "1440x1080":
		return "4:3"
	case "3:4", "896x1152", "960x1280", "1080x1440":
		return "3:4"
	case "3:2", "1344x896", "1536x1024", "1440x960":
		return "3:2"
	case "2:3", "896x1344", "1024x1536", "960x1440":
		return "2:3"
	case "4:5", "1152x1440", "896x1120":
		return "4:5"
	case "5:4", "1440x1152", "1120x896":
		return "5:4"
	case "21:9", "2560x1080", "1792x768":
		return "21:9"
	case "9:21", "1080x2560", "768x1792":
		return "9:21"
	default:
		return "1:1"
	}
}

// grokAuthModelsFetch is the payload returned by /api/models/fetch when the
// target is the local-proxy Grok Auth profile: the official model list drives
// both the suggestion chips and the enabled model definitions.
type grokAuthModelsFetch struct {
	Models         []string
	EnabledModels  []profiles.ModelDef
	DefaultModel   string
	WebSearchModel string
	Explore        string
	Plan           string
	Warning        string
}

type officialGrokModelInfo struct {
	ID                    string
	APIBackend            string
	ContextWindow         int64
	SupportsReasoningFlag bool
	ReasoningEfforts      []string
}

// fetchOfficialGrokAuthModels queries the xAI upstream /models directly with a
// pool token and rebuilds the local-proxy profile from that response, so the
// profile no longer depends on the hardcoded default model set. Entries that
// the official list no longer returns (e.g. the plan composer) are preserved
// unless they reference the retired grok-4.5.
func (s *Server) fetchOfficialGrokAuthModels(ctx context.Context, profile profiles.Profile) (grokAuthModelsFetch, error) {
	token, err := s.officialGrokToken(ctx)
	if err != nil {
		return grokAuthModelsFetch{}, err
	}
	client := &http.Client{Timeout: 20 * time.Second}
	if s.GrokPool != nil {
		if transport := s.GrokPool.Transport(); transport != nil {
			client.Transport = transport
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, grokauth.UpstreamURL()+"/models", nil)
	if err != nil {
		return grokAuthModelsFetch{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", "0.2.93")
	req.Header.Set("User-Agent", "xai-grok-workspace/0.2.93")
	resp, err := client.Do(req)
	if err != nil {
		return grokAuthModelsFetch{}, fmt.Errorf("请求官方模型列表失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return grokAuthModelsFetch{}, fmt.Errorf("读取官方模型列表失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return grokAuthModelsFetch{}, fmt.Errorf("官方模型列表返回 %s: %s", resp.Status, truncateTextServer(string(raw), 200))
	}
	official := parseOfficialGrokModels(raw)
	if len(official) == 0 {
		return grokAuthModelsFetch{}, fmt.Errorf("官方 /models 未返回任何模型")
	}

	apiKey, configured, err := s.proxyAPIKey()
	if err != nil {
		return grokAuthModelsFetch{}, err
	}
	if !configured {
		return grokAuthModelsFetch{}, fmt.Errorf("尚未导入 Grok auth JSON 或号池账号")
	}

	defs := make([]profiles.ModelDef, 0, len(official)+len(profile.Models))
	seen := map[string]bool{}
	for _, item := range official {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		def := profiles.ModelDef{
			Name:                    item.ID,
			Model:                   item.ID,
			APIBackend:              firstNonEmptyServer(item.APIBackend, "responses"),
			SupportsBackendSearch:   true,
			SupportsReasoningEffort: item.SupportsReasoningFlag,
			ReasoningEfforts:        item.ReasoningEfforts,
			ContextWindow:           500000,
			MaxCompletionTokens:     65536,
		}
		if item.ContextWindow > 0 {
			def.ContextWindow = item.ContextWindow
		}
		defs = append(defs, def)
	}
	for _, existing := range profile.Models {
		id := firstNonEmptyServer(existing.Model, existing.Name)
		if id == "" || seen[id] || id == retiredGrokAuthChatModel {
			continue
		}
		seen[id] = true
		defs = append(defs, existing)
	}

	profile.Models = cloneModelDefs(defs, s.localGrokAuthURL(), apiKey)
	names := modelNames(profile.Models)
	profile.AvailableModels = uniqueModelNames(names)
	profile.DefaultModel = reconcileGrokAuthModelRef(profile.DefaultModel, names)
	profile.WebSearchModel = reconcileGrokAuthModelRef(profile.WebSearchModel, names)
	profile.SubagentsModels.Explore = reconcileGrokAuthModelRef(profile.SubagentsModels.Explore, names)
	profile.SubagentsModels.Plan = reconcileGrokAuthModelRef(profile.SubagentsModels.Plan, names)

	result := grokAuthModelsFetch{
		Models:         profile.AvailableModels,
		EnabledModels:  profile.Models,
		DefaultModel:   profile.DefaultModel,
		WebSearchModel: profile.WebSearchModel,
		Explore:        profile.SubagentsModels.Explore,
		Plan:           profile.SubagentsModels.Plan,
	}
	updated, err := s.Profiles.Update(profile.ID, profile)
	if err != nil {
		return grokAuthModelsFetch{}, err
	}
	s.changed()
	if updated.IsActive {
		if _, activateErr := s.Switcher.Activate(updated.ID); activateErr != nil {
			result.Warning = "模型列表已按官方更新，但重写 config.toml 失败：" + activateErr.Error()
		}
	}
	return result, nil
}

func (s *Server) officialGrokToken(ctx context.Context) (string, error) {
	if s.poolConfigured() {
		token, _, err := s.GrokPool.NextToken(ctx, "models-fetch")
		if err == nil {
			return token, nil
		}
		if !s.singleGrokAuthConfigured() {
			return "", fmt.Errorf("获取号池 token 失败: %w", err)
		}
	}
	if s.singleGrokAuthConfigured() {
		token, err := s.GrokAuth.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("刷新单账号 token 失败: %w", err)
		}
		return token, nil
	}
	return "", fmt.Errorf("尚未导入 Grok auth JSON 或号池账号")
}

func parseOfficialGrokModels(body []byte) []officialGrokModelInfo {
	var payload struct {
		Data []struct {
			ID                      string `json:"id"`
			Model                   string `json:"model"`
			APIBackend              string `json:"api_backend"`
			ContextWindow           int64  `json:"context_window"`
			SupportsReasoningEffort bool   `json:"supports_reasoning_effort"`
			ReasoningEfforts        []struct {
				ID    string `json:"id"`
				Value string `json:"value"`
			} `json:"reasoning_efforts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	out := make([]officialGrokModelInfo, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := firstNonEmptyServer(item.ID, item.Model)
		if id == "" {
			continue
		}
		info := officialGrokModelInfo{
			ID:                    id,
			APIBackend:            strings.TrimSpace(item.APIBackend),
			ContextWindow:         item.ContextWindow,
			SupportsReasoningFlag: item.SupportsReasoningEffort,
		}
		for _, effort := range item.ReasoningEfforts {
			if value := firstNonEmptyServer(effort.ID, effort.Value); value != "" {
				info.ReasoningEfforts = append(info.ReasoningEfforts, value)
			}
		}
		out = append(out, info)
	}
	return out
}

// reconcileGrokAuthModelRef keeps a model reference valid after the model list
// was rebuilt from the official response: retired grok-4.5 is migrated, and a
// reference that no longer exists falls back to the first available model.
func reconcileGrokAuthModelRef(ref string, available []string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	ref = migrateGrokAuthModelID(ref)
	for _, name := range available {
		if name == ref {
			return ref
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return ref
}

func truncateTextServer(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) > limit {
		return text[:limit] + "…"
	}
	return text
}

func (s *Server) ensureGrokAuthProfile() error {
	if s.ActualPort == 0 {
		return nil
	}
	_, configured, err := s.proxyAPIKey()
	if err != nil || !configured {
		return err
	}
	_, err = s.upsertGrokAuthProfile()
	return err
}

func (s *Server) upsertGrokAuthProfile() (profiles.Profile, error) {
	apiKey, configured, err := s.proxyAPIKey()
	if err != nil {
		return profiles.Profile{}, err
	}
	if !configured {
		return profiles.Profile{}, fmt.Errorf("尚未导入 Grok auth JSON 或号池账号")
	}
	baseURL := s.localGrokAuthURL()
	list, err := s.Profiles.List()
	if err != nil {
		return profiles.Profile{}, err
	}
	var existing *profiles.Profile
	for i := range list {
		if list[i].Name == grokAuthProfileName {
			existing = &list[i]
			break
		}
	}
	profile := profiles.Profile{
		Name:            grokAuthProfileName,
		Template:        "responses",
		UpstreamFormat:  "openai_responses",
		BaseURL:         baseURL,
		APIKey:          apiKey,
		AvailableModels: modelNames(defaultGrokAuthModels),
		DefaultModel:    currentGrokAuthChatModel,
		WebSearchModel:  currentGrokAuthChatModel,
		SubagentsModels: profiles.SubagentsModels{
			Explore: currentGrokAuthChatModel,
			Plan:    "grok-composer-2.5-fast",
		},
		Models: cloneModelDefs(defaultGrokAuthModels, baseURL, apiKey),
	}
	if existing == nil {
		return s.Profiles.Create(profile)
	}
	profile.DefaultModel = migrateGrokAuthModelID(firstNonEmptyServer(existing.DefaultModel, profile.DefaultModel))
	profile.WebSearchModel = migrateGrokAuthModelID(firstNonEmptyServer(existing.WebSearchModel, profile.WebSearchModel))
	profile.SubagentsModels.Explore = migrateGrokAuthModelID(firstNonEmptyServer(existing.SubagentsModels.Explore, profile.SubagentsModels.Explore))
	profile.SubagentsModels.Plan = firstNonEmptyServer(existing.SubagentsModels.Plan, profile.SubagentsModels.Plan)
	if len(existing.Models) > 0 {
		profile.Models = cloneModelDefs(migrateGrokAuthModelDefs(existing.Models), baseURL, apiKey)
		profile.AvailableModels = uniqueModelNames(append(existing.AvailableModels, modelNames(profile.Models)...))
	}
	updated, err := s.Profiles.Update(existing.ID, profile)
	if err != nil {
		return profiles.Profile{}, err
	}
	connectionChanged := existing.BaseURL != updated.BaseURL || existing.EffectiveAPIKey() != updated.EffectiveAPIKey()
	if existing.IsActive && connectionChanged {
		return s.Switcher.Activate(updated.ID)
	}
	return updated, nil
}

func (s *Server) removeGrokAuthProfile() error {
	list, err := s.Profiles.List()
	if err != nil {
		return err
	}
	for _, profile := range list {
		if profile.Name != grokAuthProfileName {
			continue
		}
		if profile.IsActive {
			if err := s.Switcher.ActivateOfficial(); err != nil {
				return fmt.Errorf("清理当前 Grok Auth 配置: %w", err)
			}
		}
		return s.Profiles.Delete(profile.ID)
	}
	return nil
}

func (s *Server) grokAuthStatusResponse() (grokAuthResponse, error) {
	status, err := s.GrokAuth.Status()
	if err != nil {
		return grokAuthResponse{}, err
	}
	response := grokAuthResponse{
		Configured:       status.Configured,
		SingleConfigured: status.Configured,
		Email:            status.Email,
		NeedsRefresh:     status.NeedsRefresh,
		LocalAPIKey:      status.LocalAPIKey,
		Source:           status.Source,
	}
	if s.GrokPool != nil {
		pool := s.GrokPool.Status()
		if pool.Configured {
			response.Configured = true
			response.PoolAccounts = len(pool.Accounts)
			response.LocalAPIKey = pool.LocalAPIKey
			response.Source = "unified-pool"
			response.NeedsRefresh = false
			if len(pool.Accounts) == 1 {
				response.Email = pool.Accounts[0].Email
			}
		}
	}
	if !status.ExpiresAt.IsZero() {
		expiresAt := status.ExpiresAt
		response.ExpiresAt = &expiresAt
	}
	if !status.ImportedAt.IsZero() {
		importedAt := status.ImportedAt
		response.ImportedAt = &importedAt
	}
	if !status.LastRefresh.IsZero() {
		lastRefresh := status.LastRefresh
		response.LastRefresh = &lastRefresh
	}
	if response.Configured {
		response.BaseURL = s.localGrokAuthURL()
	}
	return response, nil
}

func (s *Server) localGrokAuthURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/grok/v1", s.ActualPort)
}

func (s *Server) proxyAPIKey() (string, bool, error) {
	if s.GrokPool != nil {
		status := s.GrokPool.Status()
		if status.Configured {
			return status.LocalAPIKey, true, nil
		}
	}
	if s.GrokAuth == nil {
		return "", false, nil
	}
	status, err := s.GrokAuth.Status()
	if err != nil {
		return "", false, err
	}
	return status.LocalAPIKey, status.Configured, nil
}

func (s *Server) poolConfigured() bool {
	return s.GrokPool != nil && s.GrokPool.Status().Configured
}

func (s *Server) singleGrokAuthConfigured() bool {
	if s.GrokAuth == nil {
		return false
	}
	status, err := s.GrokAuth.Status()
	return err == nil && status.Configured
}

func migrateGrokAuthModelID(id string) string {
	if id == retiredGrokAuthChatModel {
		return currentGrokAuthChatModel
	}
	return id
}

func migrateGrokAuthModelDefs(models []profiles.ModelDef) []profiles.ModelDef {
	out := make([]profiles.ModelDef, 0, len(models))
	for _, model := range models {
		if model.Model == retiredGrokAuthChatModel || (model.Model == "" && model.Name == retiredGrokAuthChatModel) {
			out = append(out, defaultGrokAuthModels[0])
			continue
		}
		out = append(out, model)
	}
	return out
}

func cloneModelDefs(models []profiles.ModelDef, baseURL, apiKey string) []profiles.ModelDef {
	out := make([]profiles.ModelDef, len(models))
	for i, model := range models {
		out[i] = model
		out[i].BaseURL = baseURL
		out[i].APIKey = apiKey
		out[i].APIBackend = "responses"
		if model.ExtraHeaders != nil {
			out[i].ExtraHeaders = make(map[string]string, len(model.ExtraHeaders))
			for key, value := range model.ExtraHeaders {
				out[i].ExtraHeaders[key] = value
			}
		}
	}
	return out
}

func modelNames(models []profiles.ModelDef) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		name := firstNonEmptyServer(model.Name, model.Model)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func uniqueModelNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func firstNonEmptyServer(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
