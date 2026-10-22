package service

import (
	"io"
	"net/http"
	"sync"
)

// openAIWSDownstreamSink lets the Messages bridge consume Responses WS events
// without writing Responses SSE to the real gin writer. A nil sink preserves
// the native /v1/responses output path; both Messages client modes consume the
// pipe through their existing SSE converters.
type openAIWSDownstreamSink struct {
	mu      sync.Mutex
	headers http.Header
	writer  io.Writer
	// Signal before the first pipe write so the reader can start without a
	// producer/consumer deadlock.
	onFirstWrite func()
}

func newOpenAIWSPipeDownstream(w io.Writer, onFirstWrite func()) *openAIWSDownstreamSink {
	return &openAIWSDownstreamSink{
		headers:      http.Header{},
		writer:       w,
		onFirstWrite: onFirstWrite,
	}
}

// SetHandshakeHeaders records request IDs, turn state and rate-limit metadata.
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

func (d *openAIWSDownstreamSink) SetHeader(key, value string) {
	if d == nil || key == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.headers.Set(key, value)
}

func (d *openAIWSDownstreamSink) HeaderSnapshot() http.Header {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.headers) == 0 {
		return nil
	}
	return d.headers.Clone()
}

// WriteFrame writes an already-framed Responses SSE event.
func (d *openAIWSDownstreamSink) WriteFrame(frame []byte) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
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

// Flush implements http.Flusher; pipe writes are already immediate.
func (d *openAIWSDownstreamSink) Flush() {}
