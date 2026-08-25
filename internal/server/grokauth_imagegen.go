package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"grok_switch/internal/grokauth"
)

// isChatCompletionsProxyPath 判断是否为 OpenAI 兼容的 chat/completions 请求。
func isChatCompletionsProxyPath(requestPath string) bool {
	suffix := strings.TrimPrefix(requestPath, "/grok/v1")
	return suffix == "/chat/completions"
}

// imageGenToolDeclaration 是注入 chat/completions 请求的 generate_image 工具
// 声明（OpenAI function calling 标准格式）。模型在 API 层即可"看到"生图工具。
func imageGenToolDeclaration() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "generate_image",
			"description": "根据文本描述生成一张图片。调用时请撰写详细的图片提示词（主体、风格、构图、光影等），并按需指定生图模式：grok-imagine-image-lite 快速、grok-imagine-image 标准（默认）、grok-imagine-image-quality 高清；宽高比支持 1:1、16:9、9:16、4:3、3:4、3:2、2:3、4:5、21:9。",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "图片提示词，可包含主体、风格、构图、光影等细节",
					},
					"model": map[string]any{
						"type":        "string",
						"enum":        []string{"grok-imagine-image-lite", "grok-imagine-image", "grok-imagine-image-quality"},
						"default":     "grok-imagine-image",
						"description": "生图模式：lite=快速，image=标准，quality=高清",
					},
					"aspect_ratio": map[string]any{
						"type":        "string",
						"enum":        []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "4:5", "21:9"},
						"default":     "1:1",
						"description": "图片宽高比",
					},
				},
				"required": []string{"prompt"},
			},
		},
	}
}

// handleChatCompletionsWithImageGen 处理 chat/completions：自动注入
// generate_image 工具声明，把模型发起的生图工具调用代为执行（账号池），
// 并把图片作为最终 assistant 消息返回，让任何 OpenAI 兼容的 Harness 无需
// 感知 MCP 即可获得生图能力。返回 handled=false 时调用方走正常代理。
func (s *Server) handleChatCompletionsWithImageGen(w http.ResponseWriter, r *http.Request, token string) (bool, error) {
	if r.Method != http.MethodPost || s.Imagine == nil {
		return false, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	_ = r.Body.Close()
	if err != nil {
		return false, nil
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return false, nil
	}
	// 流式请求暂不接管（模型不会感知工具时文本回复；保持原有流式体验）。
	if stream, _ := req["stream"].(bool); stream {
		return false, nil
	}

	tools, _ := req["tools"].([]any)
	if !containsToolDeclaration(tools, "generate_image") {
		tools = append(tools, imageGenToolDeclaration())
		req["tools"] = tools
		if _, ok := req["tool_choice"]; !ok {
			req["tool_choice"] = "auto"
		}
	}
	injected, err := json.Marshal(req)
	if err != nil {
		return false, nil
	}

	upstreamResp, err := s.forwardGrokAPI(r, token, injected)
	if err != nil {
		return true, fmt.Errorf("生图工具上游请求失败: %w", err)
	}
	defer upstreamResp.Body.Close()
	respRaw, err := io.ReadAll(io.LimitReader(upstreamResp.Body, 32<<20))
	if err != nil {
		return true, err
	}
	if upstreamResp.StatusCode >= 400 {
		copyHeader(w, upstreamResp.Header)
		w.WriteHeader(upstreamResp.StatusCode)
		_, _ = w.Write(respRaw)
		return true, nil
	}
	var chatResp map[string]any
	if err := json.Unmarshal(respRaw, &chatResp); err != nil {
		copyHeader(w, upstreamResp.Header)
		w.WriteHeader(upstreamResp.StatusCode)
		_, _ = w.Write(respRaw)
		return true, nil
	}

	toolCall, argsRaw, found := findGenerateImageCall(chatResp)
	if !found {
		// 模型没有发起生图调用（可能有第三方自己的工具调用），原样透传。
		copyHeader(w, upstreamResp.Header)
		w.WriteHeader(upstreamResp.StatusCode)
		_, _ = w.Write(respRaw)
		return true, nil
	}

	result := s.executeGenerateImageCall(r.Context(), argsRaw)
	final, err := s.buildChatCompletionsFinal(chatResp, toolCall, result)
	if err != nil {
		return true, err
	}
	finalRaw, err := json.Marshal(final)
	if err != nil {
		return true, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(finalRaw)
	return true, nil
}

// forwardGrokAPI 把（可能已注入工具的）请求体转发到官方上游，沿用本地代理
// 的鉴权与头信息。
func (s *Server) forwardGrokAPI(r *http.Request, token string, body []byte) (*http.Response, error) {
	upstream := strings.TrimRight(grokauth.UpstreamURL(), "/")
	suffix := strings.TrimPrefix(r.URL.Path, "/grok/v1")
	if suffix == "" {
		suffix = "/"
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream+suffix, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", "0.2.93")
	req.Header.Set("User-Agent", "xai-grok-workspace/0.2.93")
	client := &http.Client{}
	if s.GrokPool != nil {
		if transport := s.GrokPool.Transport(); transport != nil {
			client.Transport = transport
		}
	}
	return client.Do(req)
}

func containsToolDeclaration(tools []any, name string) bool {
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		if fnName, _ := fn["name"].(string); fnName == name {
			return true
		}
	}
	return false
}

// findGenerateImageCall 在响应里查找 generate_image 工具调用，返回该调用
// 的 tool_calls 条目与参数原始 JSON。未找到返回 found=false。
func findGenerateImageCall(chatResp map[string]any) (map[string]any, []byte, bool) {
	choices, _ := chatResp["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil, false
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	toolCalls, _ := message["tool_calls"].([]any)
	for _, tc := range toolCalls {
		call, _ := tc.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		if name, _ := fn["name"].(string); name == "generate_image" {
			args, _ := fn["arguments"].(string)
			return call, []byte(args), true
		}
	}
	return nil, nil, false
}

// executeGenerateImageCall 执行一次生图工具调用，返回聚合结果。
func (s *Server) executeGenerateImageCall(ctx context.Context, argsRaw []byte) ImagineResult {
	var params struct {
		Prompt      string `json:"prompt"`
		Model       string `json:"model"`
		AspectRatio string `json:"aspect_ratio"`
	}
	_ = json.Unmarshal(argsRaw, &params)
	prompt := strings.TrimSpace(params.Prompt)
	if prompt == "" {
		return ImagineResult{OK: false, ErrCode: "invalid_arguments", ErrMsg: "prompt 不能为空"}
	}
	model := strings.TrimSpace(params.Model)
	if model == "" {
		model = "grok-imagine-image"
	}
	aspect := strings.TrimSpace(params.AspectRatio)
	if aspect == "" {
		aspect = "1:1"
	}
	genCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	res := s.Imagine.Generate(genCtx, prompt, model, aspect)
	if res.OK && len(res.Images) > 0 {
		if dlPath, dlErr := saveImageToDownloads(filepath.Join(s.Imagine.outputsDir, path.Base(res.Images[0]))); dlErr == nil {
			res.SavedTo = dlPath
		} else {
			fmt.Fprintf(os.Stderr, "grok_switch: save image to downloads: %v\n", dlErr)
		}
	}
	return res
}

// buildChatCompletionsFinal 构造最终 assistant 响应：把生图结果写成文本 +
// 内嵌 base64 图片，并保留调用方其它（非 generate_image）工具调用。
func (s *Server) buildChatCompletionsFinal(chatResp map[string]any, executedCall map[string]any, res ImagineResult) (map[string]any, error) {
	model, _ := chatResp["model"].(string)
	if model == "" {
		model = "grok-4.6"
	}
	message := map[string]any{"role": "assistant"}
	if !res.OK {
		message["content"] = fmt.Sprintf("图片生成失败：%s（%s）。如需生图，请重试或换一个描述。", res.ErrMsg, res.ErrCode)
	} else if len(res.Images) == 0 {
		message["content"] = "图片生成失败：未收到图片数据。"
	} else {
		b64 := ""
		if data, err := os.ReadFile(filepath.Join(s.Imagine.outputsDir, path.Base(res.Images[0]))); err == nil {
			b64 = base64.StdEncoding.EncodeToString(data)
		}
		url := fmt.Sprintf("http://127.0.0.1:%d%s", s.ActualPort, res.Images[0])
		msg := fmt.Sprintf("图片已生成：%dx%d，模型 %s。", res.Width, res.Height, res.ModelName)
		if b64 != "" {
			msg += "\n\n![generated image](data:image/jpeg;base64," + b64 + ")"
		} else {
			msg += "\n\n图片地址：" + url
		}
		if res.SavedTo != "" {
			msg += "\n\n已保存到：" + res.SavedTo
		}
		message["content"] = msg
		message["images"] = []map[string]any{{
			"url":      url,
			"b64_json": b64,
		}}
	}

	// 保留除已执行之外的工具调用（调用方自己的工具）。
	remaining := otherToolCalls(chatResp, executedCall)
	if len(remaining) > 0 {
		message["tool_calls"] = remaining
		message["content"] = mapToText(message["content"]) + "\n\n（其它工具调用已原样保留，请按标准流程继续。）"
	}

	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if u, ok := chatResp["usage"].(map[string]any); ok {
		usage = u
	}
	return map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
		"usage": usage,
	}, nil
}

// otherToolCalls 返回除 executed 之外的 tool_calls 条目。
func otherToolCalls(chatResp map[string]any, executed map[string]any) []any {
	choices, _ := chatResp["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	toolCalls, _ := message["tool_calls"].([]any)
	var out []any
	for _, tc := range toolCalls {
		call, _ := tc.(map[string]any)
		if sameToolCall(call, executed) {
			continue
		}
		out = append(out, call)
	}
	return out
}

func sameToolCall(a, b map[string]any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	idA, _ := a["id"].(string)
	idB, _ := b["id"].(string)
	return idA != "" && idA == idB
}

func mapToText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func copyHeader(w http.ResponseWriter, h http.Header) {
	for k, vv := range h {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
}
