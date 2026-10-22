package service

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
)

// openAIWSDownstreamSink 把 Responses WSv2 forwarder 的下游输出从
// gin.ResponseWriter 上解耦出来。
//
// 目前只有两类使用者：
//   - /v1/responses 入站（含 fast HTTP→WS 桥接）：sink 为 nil，forwarder 直接
//     写 gin writer，行为与历史完全一致。
//   - Anthropic /v1/messages 兼容桥：sink 接管 SSE 帧 / 终态 JSON，使同一套
//     上游 WSv2 逻辑可以被重新编码成 Anthropic SSE / Messages JSON。
//
// 注意：sink 只承担“字节输出”。ops 上下文、连接复用标记等仍然通过真实的
// *gin.Context 传递，因此两条路径的请求归因保持同源。
type openAIWSDownstreamSink struct {
	mu      sync.Mutex
	headers http.Header
	wrote   bool

	// writer 非空表示流式模式：每个 SSE frame 直接写入该 writer。
	writer io.Writer
	// onFirstWrite 在第一次真正写出帧之前触发一次，用于让下游读者先就位，
	// 避免管道写入与转换器启动之间的死锁。
	onFirstWrite func()

	jsonStatus      int
	jsonContentType string
	jsonBody        []byte
}

func newOpenAIWSPipeDownstream(w io.Writer, onFirstWrite func()) *openAIWSDownstreamSink {
	return &openAIWSDownstreamSink{
		headers:      http.Header{},
		writer:       w,
		onFirstWrite: onFirstWrite,
	}
}

func newOpenAIWSCollectDownstream() *openAIWSDownstreamSink {
	return &openAIWSDownstreamSink{headers: http.Header{}}
}

// SetHandshakeHeaders 记录上游 WS 握手响应头，供下游转换器读取
// x-request-id / x-codex-turn-state 等元信息。
func (d *openAIWSDownstreamSink) SetHandshakeHeaders(headers http.Header) {
	if d == nil || len(headers) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, values := range headers {
		d.headers[key] = append([]string(nil), values...)
	}
}

// SetHeader 覆盖单个下游可见头（例如 WS 握手返回的 turn state）。
func (d *openAIWSDownstreamSink) SetHeader(key, value string) {
	if d == nil || key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.headers.Set(key, value)
}

// HeaderSnapshot 返回当前头的浅拷贝，调用方可以安全读取。
func (d *openAIWSDownstreamSink) HeaderSnapshot() http.Header {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.headers) == 0 {
		return nil
	}
	out := make(http.Header, len(d.headers))
	for key, values := range d.headers {
		out[key] = append([]string(nil), values...)
	}
	return out
}

// WriteFrame 写出一帧 SSE（已包含 "data: ...\n\n" 包装）。
func (d *openAIWSDownstreamSink) WriteFrame(frame []byte) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	d.wrote = true
	writer := d.writer
	onFirstWrite := d.onFirstWrite
	d.onFirstWrite = nil
	d.mu.Unlock()
	if onFirstWrite != nil {
		onFirstWrite()
	}
	if writer == nil {
		return nil
	}
	_, err := writer.Write(frame)
	return err
}

// Flush 实现 http.Flusher：管道写入本身即时，无需额外刷新。
func (d *openAIWSDownstreamSink) Flush() {}

// Written 报告是否已经产出过下游字节。
func (d *openAIWSDownstreamSink) Written() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.wrote
}

// WriteJSON 记录非流式终态响应体（状态码 + Content-Type + body）。
func (d *openAIWSDownstreamSink) WriteJSON(status int, contentType string, body []byte) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wrote = true
	d.jsonStatus = status
	d.jsonContentType = contentType
	d.jsonBody = append([]byte(nil), body...)
}

// CollectedJSON 返回 WriteJSON 记录的终态响应。
func (d *openAIWSDownstreamSink) CollectedJSON() (status int, contentType string, body []byte) {
	if d == nil {
		return 0, "", nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.jsonStatus, d.jsonContentType, append([]byte(nil), d.jsonBody...)
}

// writeWSJSONDownstream 按“是否由 sink 接管下游”写出 JSON 响应。
func writeWSJSONDownstream(c *gin.Context, sink *openAIWSDownstreamSink, status int, payload any) {
	if sink != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			body = []byte(`{"error":{"type":"upstream_error","message":"failed to encode upstream error payload"}}`)
		}
		sink.WriteJSON(status, "application/json", body)
		return
	}
	if c != nil {
		c.JSON(status, payload)
	}
}
