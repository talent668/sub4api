package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIWSIngressReaderContextKey = "openai_ws_ingress_control_reader"

var errOpenAIWSClientTurnCancelled = errors.New("websocket turn cancelled by client")

// IsOpenAIWSClientTurnCancelled distinguishes a user stop from an upstream
// failure. A cancelled turn must never enter account failover/replay.
func IsOpenAIWSClientTurnCancelled(err error) bool {
	return errors.Is(err, errOpenAIWSClientTurnCancelled)
}

type openAIWSIngressControlTurn struct {
	responseID    string
	active        bool
	cancelled     bool
	cancelPayload []byte
	sendCancel    func([]byte) error
	abort         context.CancelCauseFunc
	watchdog      *time.Timer
}

// openAIWSIngressControlReader owns the only reader of a serial-mode client
// socket. Its lifetime spans account attempts, rather than one upstream turn.
type openAIWSIngressControlReader struct {
	conn              *coderws.Conn
	ctx               context.Context
	writeControl      func([]byte) error
	frames            chan []byte
	done              chan struct{}
	mu                sync.Mutex
	turn              *openAIWSIngressControlTurn
	err               error
	pendingBytesLimit int64
}

type openAIWSIngressControlContextKey struct{}
type openAIWSIngressControlContext struct {
	reader *openAIWSIngressControlReader
	turn   *openAIWSIngressControlTurn
}

func cachedOpenAIWSIngressControlReader(c *gin.Context, conn *coderws.Conn) *openAIWSIngressControlReader {
	if c == nil {
		return nil
	}
	value, ok := c.Get(openAIWSIngressReaderContextKey)
	if !ok {
		return nil
	}
	reader, _ := value.(*openAIWSIngressControlReader)
	if reader != nil && reader.conn == conn {
		return reader
	}
	return nil
}

func openAIWSIngressControlReaderFor(ctx context.Context, c *gin.Context, conn *coderws.Conn, writeTimeout time.Duration, pendingLimit int64) *openAIWSIngressControlReader {
	if existing := cachedOpenAIWSIngressControlReader(c, conn); existing != nil {
		return existing
	}
	if writeTimeout <= 0 {
		writeTimeout = 2 * time.Second
	}
	if pendingLimit <= 0 {
		pendingLimit = openAIWSClientReadLimitBytesDefault
	}
	r := &openAIWSIngressControlReader{conn: conn, ctx: ctx, frames: make(chan []byte, 1), done: make(chan struct{}), turn: &openAIWSIngressControlTurn{active: true}, pendingBytesLimit: pendingLimit}
	r.writeControl = func(payload []byte) error {
		writeCtx, cancel := context.WithTimeout(ctx, min(writeTimeout, 2*time.Second))
		defer cancel()
		return conn.Write(writeCtx, coderws.MessageText, payload)
	}
	if c != nil {
		c.Set(openAIWSIngressReaderContextKey, r)
	}
	go r.readLoop()
	return r
}

func (r *openAIWSIngressControlReader) readLoop() {
	defer close(r.done)
	for {
		// Cancellation of a read context closes coder/websocket's transport.
		// Session close/idle/lease-loss paths send their close frame first and
		// join this sole reader via done, rather than spawning another reader.
		kind, payload, err := r.conn.Read(context.WithoutCancel(r.ctx))
		if err != nil {
			r.mu.Lock()
			r.err = err
			r.mu.Unlock()
			return
		}
		if kind != coderws.MessageText && kind != coderws.MessageBinary {
			continue
		}
		eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
		if eventType == "response.cancel" && gjson.ValidBytes(payload) {
			if err := r.cancelCurrent(payload); err != nil {
				if !r.reject(err.Error()) {
					return
				}
			}
			continue
		}
		if eventType != "" && eventType != "response.create" {
			if !r.reject(fmt.Sprintf("unsupported websocket request type: %s", truncateString(eventType, 64))) {
				return
			}
			continue
		}
		if int64(len(payload)) > r.pendingBytesLimit {
			if !r.reject("queued response.create exceeds the configured websocket frame limit") {
				return
			}
			continue
		}
		select {
		case r.frames <- payload:
			// The current turn remains current until readNext dequeues this
			// frame after terminal delivery and AfterTurn. Controls never queue.
		default:
			if !r.reject("only one pending response.create is supported; wait for the active response") {
				return
			}
		}
	}
}

func (r *openAIWSIngressControlReader) reject(message string) bool {
	payload, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "code": "invalid_control_frame", "message": message}})
	if err := r.writeControl(payload); err != nil {
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
		return false
	}
	return true
}

func (r *openAIWSIngressControlReader) cancelCurrent(payload []byte) error {
	if len(payload) > 4096 {
		return errors.New("response.cancel control frame exceeds 4 KiB")
	}
	if !gjson.ValidBytes(payload) {
		return errors.New("invalid response.cancel JSON")
	}
	responseField := gjson.GetBytes(payload, "response_id")
	if responseField.Exists() && responseField.Type != gjson.String {
		return errors.New("response_id must be a string")
	}
	requested := strings.TrimSpace(responseField.String())
	if len(requested) > 256 {
		return errors.New("response_id is too long")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.turn
	if t == nil || !t.active {
		return errors.New("no active response to cancel")
	}
	if requested != "" && (t.responseID == "" || requested != t.responseID) {
		return errors.New("response_id does not match the active response on this socket")
	}
	if t.cancelled {
		return nil
	}
	t.cancelled = true
	markWSFallbackCancelled(r.ctx)
	control := map[string]string{"type": "response.cancel"}
	if requested != "" {
		control["response_id"] = requested
	} else if t.responseID != "" {
		control["response_id"] = t.responseID
	}
	t.cancelPayload, _ = json.Marshal(control)
	return r.dispatchCancelLocked(t)
}

func (r *openAIWSIngressControlReader) dispatchCancelLocked(t *openAIWSIngressControlTurn) error {
	if t.sendCancel != nil {
		if err := t.sendCancel(t.cancelPayload); err != nil {
			if t.abort != nil {
				t.abort(errOpenAIWSClientTurnCancelled)
			}
			return errors.New("upstream cancellation could not be confirmed; reconnect to continue")
		}
		// An unresponsive upstream cannot retain a cancelled request forever.
		// Only this turn/upstream context is cancelled, never the client reader.
		t.watchdog = time.AfterFunc(2*time.Second, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.turn == t && t.active && t.abort != nil {
				t.abort(errOpenAIWSClientTurnCancelled)
			}
		})
	} else if t.abort != nil {
		t.abort(errOpenAIWSClientTurnCancelled)
	}
	return nil
}

func (r *openAIWSIngressControlReader) currentTurn() *openAIWSIngressControlTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turn == nil || !r.turn.active {
		r.turn = &openAIWSIngressControlTurn{active: true}
	}
	return r.turn
}

func (r *openAIWSIngressControlReader) install(t *openAIWSIngressControlTurn, send func([]byte) error, abort context.CancelCauseFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turn != t || !t.active {
		return
	}
	t.sendCancel = send
	t.abort = abort
	if t.cancelled {
		_ = r.dispatchCancelLocked(t)
	}
}

func (r *openAIWSIngressControlReader) detach(t *openAIWSIngressControlTurn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t.watchdog != nil {
		t.watchdog.Stop()
		t.watchdog = nil
	}
	t.sendCancel = nil
	t.abort = nil
}

func (r *openAIWSIngressControlReader) cancelled(t *openAIWSIngressControlTurn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return t != nil && t.cancelled
}

func (r *openAIWSIngressControlReader) writeTurn(t *openAIWSIngressControlTurn, payload []byte, write func([]byte) error) error {
	eventType, responseID, _ := parseOpenAIWSEventEnvelope(payload)
	r.mu.Lock()
	if r.turn == t && responseID != "" && t.responseID == "" {
		t.responseID = responseID
	}
	if isOpenAIWSTerminalEvent(eventType) || (eventType == "error" && gjson.GetBytes(payload, "error.code").String() == "response_cancelled") {
		// A client may submit its next create as soon as it sees the terminal.
		// Serialize that admission with the completed terminal write.
		err := write(payload)
		t.active = false
		t.sendCancel = nil
		t.abort = nil
		if t.watchdog != nil {
			t.watchdog.Stop()
			t.watchdog = nil
		}
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()
	return write(payload)
}

func (r *openAIWSIngressControlReader) readNext(ctx context.Context, idle time.Duration) ([]byte, error) {
	var timeout <-chan time.Time
	var timer *time.Timer
	if idle > 0 {
		timer = time.NewTimer(idle)
		defer timer.Stop()
		timeout = timer.C
	}
	closeAndJoin := func(status coderws.StatusCode, reason string, cause error) ([]byte, error) {
		_ = r.conn.Close(status, reason)
		_ = r.conn.CloseNow()
		<-r.done
		return nil, NewOpenAIWSClientCloseError(status, reason, cause)
	}
	select {
	case payload := <-r.frames:
		if ctx.Err() != nil {
			cause := context.Cause(ctx)
			if errors.Is(cause, ErrOpenAIWSIngressLeaseLost) {
				return closeAndJoin(coderws.StatusTryAgainLater, "websocket ingress capacity lease lost; please reconnect", cause)
			}
			return closeAndJoin(coderws.StatusGoingAway, "websocket request canceled", cause)
		}
		r.mu.Lock()
		if r.err != nil {
			err := r.err
			r.mu.Unlock()
			return nil, err
		}
		if r.turn != nil && r.turn.active {
			r.mu.Unlock()
			return nil, errors.New("cannot start a pending response before the active turn finishes")
		}
		r.turn = &openAIWSIngressControlTurn{active: true}
		r.mu.Unlock()
		return payload, nil
	case <-r.done:
		r.mu.Lock()
		err := r.err
		r.mu.Unlock()
		if err == nil {
			err = context.Canceled
		}
		return nil, err
	case <-ctx.Done():
		cause := context.Cause(ctx)
		if errors.Is(cause, ErrOpenAIWSIngressLeaseLost) {
			return closeAndJoin(coderws.StatusTryAgainLater, "websocket ingress capacity lease lost; please reconnect", cause)
		}
		return closeAndJoin(coderws.StatusGoingAway, "websocket request canceled", cause)
	case <-timeout:
		return closeAndJoin(coderws.StatusNormalClosure, "websocket idle timeout", context.DeadlineExceeded)
	}
}

func withOpenAIWSIngressControl(ctx context.Context, r *openAIWSIngressControlReader, t *openAIWSIngressControlTurn) context.Context {
	return context.WithValue(ctx, openAIWSIngressControlContextKey{}, openAIWSIngressControlContext{r, t})
}
func openAIWSIngressControlFromContext(ctx context.Context) (openAIWSIngressControlContext, bool) {
	v, ok := ctx.Value(openAIWSIngressControlContextKey{}).(openAIWSIngressControlContext)
	return v, ok
}
func openAIWSIngressTurnCancelled(ctx context.Context) bool {
	v, ok := openAIWSIngressControlFromContext(ctx)
	return ok && v.reader.cancelled(v.turn)
}

func openAIWSCancellationUsage(payload []byte) (json.RawMessage, bool) {
	value := gjson.GetBytes(payload, "response.usage")
	if !value.IsObject() {
		value = gjson.GetBytes(payload, "usage")
	}
	if !value.IsObject() {
		return nil, false
	}
	input, output := value.Get("input_tokens"), value.Get("output_tokens")
	return json.RawMessage(value.Raw), input.Type == gjson.Number && output.Type == gjson.Number
}

func openAIWSTerminalCompletedBeforeCancel(eventType string, payload []byte) bool {
	status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.status").String()))
	if status == "completed" {
		return eventType == "response.completed" || eventType == "response.done"
	}
	return eventType == "response.completed" && status == ""
}

func openAIWSClientCancellationTerminal(payload []byte) string {
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.status").String())) {
	case "cancelled", "canceled":
		return "response.cancelled"
	default:
		return "response.incomplete"
	}
}

// A bridge cancelled before its final response cannot safely reconstruct the
// old tool/response lineage. Only message-only input can begin a fresh turn on
// the same client socket; all connection-scoped items require a reconnect.
func openAIWSFreshRequestAfterCancel(payload []byte) bool {
	if strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != "" {
		return false
	}
	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || input.Type == gjson.Null || input.Type == gjson.String {
		return true
	}
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		kind := strings.TrimSpace(item.Get("type").String())
		if kind != "message" && (kind != "" || item.Get("role").Type != gjson.String || !item.Get("content").Exists()) {
			return false
		}
	}
	return true
}

func buildOpenAIWSClientCancelledEvent(responseID, model string, usage json.RawMessage) []byte {
	if responseID == "" {
		payload, _ := json.Marshal(map[string]any{"type": "error", "status": http.StatusConflict, "error": map[string]string{"type": "invalid_request_error", "code": "response_cancelled", "message": "request cancelled before an upstream response was created"}})
		return payload
	}
	response := map[string]any{"id": responseID, "object": "response", "model": model, "status": "incomplete", "incomplete_details": map[string]string{"reason": "interrupted"}, "output": []any{}}
	if len(usage) > 0 {
		response["usage"] = usage
	}
	payload, _ := json.Marshal(map[string]any{"type": "response.incomplete", "response": response})
	return payload
}
