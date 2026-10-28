// Package mcpserver 实现一个轻量的 MCP (Model Context Protocol) stdio 服务器，
// 把 grok_switch 的生图能力暴露为标准的 generate_image 工具。
//
// 它作为独立子进程运行（`grok_switch mcp`），通过本机 HTTP 调用主进程的
// /api/imagine/generate 完成生图，因此不依赖 server/agentbridge 包，避免循环引用。
package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Run 以 MCP stdio 服务器身份执行（由主进程以 `grok_switch mcp` 拉起）。
// baseURL 来自环境变量 GROK_SWITCH_BASE_URL，缺省为本机默认面板端口。
// 正常退出返回 0，仅当读取输入失败时返回 1。
func Run(_ []string) int {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("GROK_SWITCH_BASE_URL")), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:17878"
	}
	s := &server{
		baseURL:    baseURL,
		client:     &http.Client{Timeout: 150 * time.Second},
		in:         bufio.NewReaderSize(os.Stdin, 1<<20),
		stdin:      os.Stdin,
		out:        bufio.NewWriterSize(os.Stdout, 1<<20),
		serverName: "image_generator",
		serverVer:  "1.0.0",
	}
	if err := s.loop(); err != nil {
		return 1
	}
	return 0
}

// server 是 MCP stdio 服务器的核心。
type server struct {
	baseURL    string
	client     *http.Client
	in         *bufio.Reader
	stdin      *os.File // 父进程关掉 stdin 时用来取消轮询；测试不填则不监视。
	jobWait    time.Duration
	out        *bufio.Writer
	protocol   string
	serverName string
	serverVer  string
}

// rpcRequest / rpcResponse 是 JSON-RPC 2.0 消息。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func okJSON(id json.RawMessage, result any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errJSON(code int, message string, id json.RawMessage) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

// loop 逐行读取 stdin 的 JSON-RPC 消息并回复。
func (s *server) loop() error {
	for {
		line, err := s.in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.handleLine(line)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (s *server) handleLine(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.write(errJSON(-32700, "Parse error", nil))
		return
	}
	var id json.RawMessage
	if len(req.ID) == 0 || string(req.ID) == "null" {
		id = nil
	} else {
		id = req.ID
	}
	switch req.Method {
	case "initialize":
		s.write(okJSON(id, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.serverName, "version": s.serverVer},
		}))
	case "notifications/initialized", "notifications/cancelled", "notifications/progress":
		// 通知无需响应。
	case "ping":
		s.write(okJSON(id, map[string]any{}))
	case "tools/list":
		s.write(okJSON(id, map[string]any{"tools": []any{generateImageToolDef()}}))
	case "tools/call":
		s.handleToolCall(id, req.Params)
	case "resources/list":
		s.write(okJSON(id, map[string]any{"resources": []any{}}))
	case "prompts/list":
		s.write(okJSON(id, map[string]any{"prompts": []any{}}))
	default:
		s.write(errJSON(-32601, "Method not found: "+req.Method, id))
	}
}

func (s *server) write(resp rpcResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = s.out.Write(data)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}

// generateImageToolDef 返回工具定义（JSON Schema 参数）。
func generateImageToolDef() map[string]any {
	return map[string]any{
		"name":        "generate_image",
		"description": "根据文本描述生成一张图片。调用时请撰写详细的图片提示词（主体、风格、构图、光影等），并按需指定生图模式：grok-imagine-image-lite 快速、grok-imagine-image 标准（默认）、grok-imagine-image-quality 高清；宽高比支持 1:1、16:9、9:16、4:3、3:4、3:2、2:3、4:5、21:9。返回生成的图片及其本地 URL。",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "图片提示词，可包含主体、风格、构图、光影等细节",
				},
				"model": map[string]any{
					"type":        "string",
					"description": "生图模式：grok-imagine-image-lite=快速，grok-imagine-image=标准（默认），grok-imagine-image-quality=高清",
					"enum":        []string{"grok-imagine-image-lite", "grok-imagine-image", "grok-imagine-image-quality"},
					"default":     "grok-imagine-image",
				},
				"aspect_ratio": map[string]any{
					"type":        "string",
					"description": "图片宽高比",
					"enum":        []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "4:5", "5:4", "21:9", "9:21"},
					"default":     "1:1",
				},
			},
			"required": []string{"prompt"},
		},
	}
}

type toolCallRequest struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleToolCall 执行工具并返回 MCP content 块。
func (s *server) handleToolCall(id json.RawMessage, params json.RawMessage) {
	var call toolCallRequest
	if err := json.Unmarshal(params, &call); err != nil {
		s.write(errJSON(-32602, "Invalid tool call params", id))
		return
	}
	if call.Name != "generate_image" {
		s.write(errJSON(-32602, "Unknown tool: "+call.Name, id))
		return
	}
	contents, isError := s.generateImage(call.Arguments)
	s.write(okJSON(id, map[string]any{
		"content": contents,
		"isError": isError,
	}))
}

// generateImage 调用主进程生图并把图片转成 MCP content。
// 成功时返回 [image, text] 两个 content 块；失败时返回单个 text content 且 isError 为 true。
func (s *server) generateImage(args json.RawMessage) ([]map[string]any, bool) {
	var params struct {
		Prompt      string `json:"prompt"`
		Model       string `json:"model"`
		AspectRatio string `json:"aspect_ratio"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return []map[string]any{textContent("参数解析失败: " + err.Error())}, true
	}
	prompt := strings.TrimSpace(params.Prompt)
	if prompt == "" {
		return []map[string]any{textContent("prompt 不能为空")}, true
	}
	model := strings.TrimSpace(params.Model)
	if model == "" {
		model = "grok-imagine-image"
	}
	aspect := strings.TrimSpace(params.AspectRatio)
	if aspect == "" {
		aspect = "1:1"
	}

	body, _ := json.Marshal(map[string]any{
		"prompt":       prompt,
		"model":        model,
		"aspect_ratio": aspect,
	})
	ctx, cancel := context.WithTimeout(context.Background(), s.jobTimeout())
	defer cancel()
	stopWatch := watchStdinClosed(ctx, s.stdin, cancel)
	defer stopWatch()
	resp, err := s.postJSON(ctx, s.baseURL+"/api/imagine/generate", body)
	if err != nil {
		if ctx.Err() != nil {
			return []map[string]any{textContent(imagineWaitErr(ctx).Error())}, true
		}
		return []map[string]any{textContent("生图服务不可用: " + err.Error())}, true
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return []map[string]any{textContent("读取生图响应失败: " + err.Error())}, true
	}
	var result struct {
		OK        bool     `json:"ok"`
		Images    []string `json:"images"`
		ModelName string   `json:"model_name"`
		Width     int      `json:"width"`
		Height    int      `json:"height"`
		ErrCode   string   `json:"err_code"`
		ErrMsg    string   `json:"err_msg"`
		Job       struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return []map[string]any{textContent("解析生图响应失败: " + err.Error())}, true
	}
	imageURL := ""
	modelName := result.ModelName
	width, height := result.Width, result.Height
	// /api/imagine/generate 现在立刻返回任务，图片在 /api/imagine/jobs 里。
	// 只看这一次响应的话 images 为空，工具会把正在生成的图报成失败。
	if result.OK && result.Job.ID != "" && len(result.Images) == 0 {
		job, err := s.waitImagineJob(ctx, result.Job.ID)
		if err != nil {
			return []map[string]any{textContent(err.Error())}, true
		}
		if job.State == "error" || len(job.Images) == 0 {
			msg := job.Error
			if msg == "" {
				msg = "生图失败"
			}
			return []map[string]any{textContent(msg)}, true
		}
		imageURL = job.Images[0].URL
		modelName = job.Images[0].Model
		width, height = job.Images[0].Width, job.Images[0].Height
	} else if resp.StatusCode >= 400 || !result.OK || len(result.Images) == 0 {
		msg := result.ErrMsg
		if msg == "" {
			msg = fmt.Sprintf("生图失败（HTTP %d）", resp.StatusCode)
		}
		return []map[string]any{textContent(msg)}, true
	} else {
		imageURL = result.Images[0]
	}

	// 下载图片并转 base64，让模型在对话中直接看到结果。
	data, err := s.fetchBytes(ctx, imageURL)
	if err != nil {
		return []map[string]any{textContent("下载图片失败: " + err.Error())}, true
	}
	mimeType := "image/jpeg"
	if strings.HasSuffix(strings.ToLower(imageURL), ".png") {
		mimeType = "image/png"
	}
	text := fmt.Sprintf("图片已生成：%s（%dx%d，模型 %s）", s.baseURL+imageURL, width, height, modelName)
	return []map[string]any{
		map[string]any{
			"type":     "image",
			"data":     base64.StdEncoding.EncodeToString(data),
			"mimeType": mimeType,
		},
		textContent(text),
	}, false
}

type imagineJobView struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Error  string `json:"error"`
	Images []struct {
		URL    string `json:"url"`
		Model  string `json:"model"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"images"`
}

// waitImagineJob 轮询任务直到结束。生成接口只负责建任务，图片要等终态才有。
// ctx 到期或父进程关掉 stdin 时停止，不再干等满 140 秒。
func (s *server) waitImagineJob(ctx context.Context, id string) (imagineJobView, error) {
	for {
		if err := ctx.Err(); err != nil {
			return imagineJobView{}, imagineWaitErr(ctx)
		}
		resp, err := s.get(ctx, s.baseURL+"/api/imagine/jobs")
		if err != nil {
			if ctx.Err() != nil {
				return imagineJobView{}, imagineWaitErr(ctx)
			}
			return imagineJobView{}, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return imagineJobView{}, readErr
		}
		if resp.StatusCode >= 400 {
			return imagineJobView{}, fmt.Errorf("查询生图任务失败（HTTP %d）", resp.StatusCode)
		}
		var payload struct {
			Jobs []imagineJobView `json:"jobs"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return imagineJobView{}, err
		}
		for _, job := range payload.Jobs {
			if job.ID != id || job.State == "" || job.State == "running" {
				continue
			}
			return job, nil
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return imagineJobView{}, imagineWaitErr(ctx)
		case <-timer.C:
		}
	}
}

func (s *server) jobTimeout() time.Duration {
	if s.jobWait > 0 {
		return s.jobWait
	}
	return 140 * time.Second
}

func imagineWaitErr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("生图任务超时")
	}
	return fmt.Errorf("生图已取消")
}

// watchStdinClosed 在父进程关闭 stdin 时取消轮询。bufio.Reader 不能并发读，
// 所以这里只监视挂起，不从缓冲里取字节。stdin 为空时不监视。
func watchStdinClosed(ctx context.Context, stdin *os.File, cancel context.CancelFunc) func() {
	if stdin == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		noticeStdinClosed(ctx, stdin, cancel)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (s *server) postJSON(ctx context.Context, url string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return s.client.Do(req)
}

func (s *server) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

func textContent(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func (s *server) fetchBytes(ctx context.Context, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "/") {
		return nil, fmt.Errorf("非本机图片路径: %s", url)
	}
	resp, err := s.get(ctx, s.baseURL+url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}
