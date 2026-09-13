package webpool

// h2chrome.go — Chrome 指纹的 HTTP/2 客户端。
//
// CF 的第二道门是 h2 帧指纹：x/net/http2 的 SETTINGS/WINDOW_UPDATE 与
// Chrome 差异明显（INITIAL_WINDOW_SIZE、帧序、PRIORITY），实测 403。
// 这里用 http2.Framer 手写 h2 连接建立与单请求生命周期，帧序列对齐
// Chrome 抓包：
//
//	连接前言 → SETTINGS(6 项) → WINDOW_UPDATE(conn) → PRIORITY(1)
//	→ HEADERS(1, END_HEADERS|PRIORITY, weight=256) → DATA(1, END_STREAM)
//	→ 读响应帧流（HEADERS/DATA/SETTINGS/WINDOW_UPDATE/PING/GOAWAY）

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// chromeSettings 与 Chrome 的 h2 SETTINGS 对齐（Akamai 指纹关键项）。
var chromeSettings = []http2.Setting{
	{ID: http2.SettingHeaderTableSize, Val: 65536},
	{ID: http2.SettingEnablePush, Val: 0},
	{ID: http2.SettingMaxConcurrentStreams, Val: 1000},
	{ID: http2.SettingInitialWindowSize, Val: 6291456},
	{ID: http2.SettingMaxFrameSize, Val: 16384},
	{ID: http2.SettingMaxHeaderListSize, Val: 262144},
}

// chromeConnWindowInc 是 Chrome 的连接级 WINDOW_UPDATE 增量。
const chromeConnWindowInc = 15663105

// chromeH2Transport 实现 http.RoundTripper：每请求新建连接（Chrome
// 对 grok.com 也是每请求新连接），流式读响应。
type chromeH2Transport struct {
	dialTLS func(ctx context.Context, network, addr string) (net.Conn, error)
}

// RoundTrip 执行一次 Chrome 指纹 h2 请求。
func (t *chromeH2Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	addr := req.URL.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	conn, err := t.dialTLS(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("Chrome TLS 连接失败: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()

	// 1. 连接前言。
	if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
		_ = conn.Close()
		return nil, err
	}

	bw := bufio.NewWriter(conn)
	br := bufio.NewReader(conn)
	framer := http2.NewFramer(bw, br)

	// 2. Chrome SETTINGS + 3. WINDOW_UPDATE + 4. PRIORITY。
	if err := framer.WriteSettings(chromeSettings...); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := framer.WriteWindowUpdate(0, chromeConnWindowInc); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := framer.WritePriority(1, http2.PriorityParam{Weight: 255}); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// 5. HEADERS（Chrome 顺序编码）。
	headerBlock, err := encodeChromeHeaders(req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	hasBody := req.Body != nil
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      1,
		BlockFragment: headerBlock,
		EndHeaders:    true,
		Priority:      http2.PriorityParam{Weight: 255},
	}); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// 6. DATA 帧（请求体）。
	if hasBody {
		body, readErr := io.ReadAll(req.Body)
		if readErr != nil {
			_ = conn.Close()
			return nil, readErr
		}
		if len(body) > 0 {
			if err := framer.WriteData(1, true, body); err != nil {
				_ = conn.Close()
				return nil, err
			}
		} else {
			// 空体也要发 END_STREAM（空 DATA 帧）。
			if err := framer.WriteData(1, true, nil); err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
	}
	if err := bw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	// 7. 读响应帧流（后台 goroutine → pipe）。
	return readH2Response(framer, bw, conn, req)
}

// encodeChromeHeaders 按 Chrome 的头序用 HPACK 编码请求头。
func encodeChromeHeaders(req *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	// 伪头顺序：:method :authority :scheme :path（Chrome 抓包序）。
	if err := enc.WriteField(hpack.HeaderField{Name: ":method", Value: req.Method}); err != nil {
		return nil, err
	}
	host := req.URL.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	} else if strings.HasSuffix(host, ":443") {
		host = strings.TrimSuffix(host, ":443")
	}
	if err := enc.WriteField(hpack.HeaderField{Name: ":authority", Value: host}); err != nil {
		return nil, err
	}
	if err := enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"}); err != nil {
		return nil, err
	}
	path := req.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	if err := enc.WriteField(hpack.HeaderField{Name: ":path", Value: path}); err != nil {
		return nil, err
	}
	// Chrome 常见顺序：content-length → 常规头（保持 req.Header 的键序）。
	if req.ContentLength > 0 {
		if err := enc.WriteField(hpack.HeaderField{Name: "content-length", Value: fmt.Sprintf("%d", req.ContentLength)}); err != nil {
			return nil, err
		}
	}
	for key, values := range req.Header {
		lower := strings.ToLower(key)
		if lower == "host" || lower == "content-length" {
			continue
		}
		for _, v := range values {
			if err := enc.WriteField(hpack.HeaderField{Name: lower, Value: v}); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}

// h2ResponseBody 是流式读 h2 DATA 帧的响应体。
type h2ResponseBody struct {
	pipe     *io.PipeReader
	conn     net.Conn
	closeOne sync.Once
}

func (b *h2ResponseBody) Read(p []byte) (int, error) { return b.pipe.Read(p) }
func (b *h2ResponseBody) Close() error {
	b.closeOne.Do(func() {
		_ = b.pipe.Close()
		_ = b.conn.Close()
	})
	return nil
}

// readH2Response 读 h2 响应帧流，返回流式 http.Response。
func readH2Response(framer *http2.Framer, bw *bufio.Writer, conn net.Conn, req *http.Request) (*http.Response, error) {
	decoder := hpack.NewDecoder(4096, nil)
	pipeReader, pipeWriter := io.Pipe()
	body := &h2ResponseBody{pipe: pipeReader, conn: conn}

	type responseMeta struct {
		statusCode int
		header     http.Header
	}
	metaCh := make(chan responseMeta, 1)

	// 后台 goroutine 持续读帧。
	go func() {
		defer pipeWriter.Close()
		header := make(http.Header)
		statusCode := 0
		metaSent := false
		sendMeta := func() {
			if !metaSent && statusCode > 0 {
				select {
				case metaCh <- responseMeta{statusCode: statusCode, header: header}:
				default:
				}
				metaSent = true
			}
		}
		for {
			frame, err := framer.ReadFrame()
			if err != nil {
				sendMeta() // 部分响应也可用
				return
			}
			switch f := frame.(type) {
			case *http2.HeadersFrame:
				hdrs, decodeErr := decoder.DecodeFull(f.HeaderBlockFragment())
				if decodeErr != nil {
					sendMeta()
					return
				}
				for _, h := range hdrs {
					if h.Name == ":status" {
						fmt.Sscanf(h.Value, "%d", &statusCode)
						continue
					}
					header.Add(h.Name, h.Value)
				}
				sendMeta()
			case *http2.DataFrame:
				if len(f.Data()) > 0 {
					if _, writeErr := pipeWriter.Write(f.Data()); writeErr != nil {
						sendMeta()
						return
					}
				}
				if f.StreamEnded() {
					sendMeta()
					return
				}
			case *http2.SettingsFrame:
				if f.IsAck() {
					continue
				}
				// 服务器 SETTINGS：发 ACK。
				_ = framer.WriteSettingsAck()
				_ = bw.Flush()
			case *http2.PingFrame:
				if !f.IsAck() {
					_ = framer.WritePing(true, f.Data)
					_ = bw.Flush()
				}
			case *http2.GoAwayFrame:
				sendMeta()
				return
			case *http2.RSTStreamFrame:
				if f.StreamID == 1 {
					sendMeta()
					return
				}
			case *http2.WindowUpdateFrame, *http2.PriorityFrame:
				// 忽略。
			}
		}
	}()

	// 等 HEADERS 帧（含 :status）到达；超时 30s。
	select {
	case meta := <-metaCh:
		resp := &http.Response{
			Proto:      "HTTP/2.0",
			ProtoMajor: 2,
			ProtoMinor: 0,
			Status:     fmt.Sprintf("%d %s", meta.statusCode, http.StatusText(meta.statusCode)),
			StatusCode: meta.statusCode,
			Header:     meta.header,
			Body:       body,
			Request:    req,
		}
		if cl := meta.header.Get("Content-Length"); cl != "" {
			var contentLength int64
			fmt.Sscanf(cl, "%d", &contentLength)
			resp.ContentLength = contentLength
		}
		return resp, nil
	case <-time.After(30 * time.Second):
		body.Close()
		return nil, fmt.Errorf("等待 h2 响应头超时")
	}
}

var _ http.RoundTripper = (*chromeH2Transport)(nil)
var _ = url.URL{}
