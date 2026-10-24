package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSIngressControlCancelScopeAndIdempotency(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	target := &openAIWSIngressControlTurn{active: true, responseID: "resp_one"}
	r := &openAIWSIngressControlReader{ctx: ctx, turn: target}
	writes := 0
	r.install(target, func([]byte) error { writes++; return nil }, func(error) {})
	require.Error(t, r.cancelCurrent([]byte(`{"type":"response.cancel","response_id":"other"}`)))
	require.Zero(t, writes)
	require.NoError(t, r.cancelCurrent([]byte(`{"type":"response.cancel","response_id":"resp_one"}`)))
	require.NoError(t, r.cancelCurrent([]byte(`{"type":"response.cancel","response_id":"resp_one"}`)))
	require.Equal(t, 1, writes)
	require.NoError(t, r.writeTurn(target, []byte(`{"type":"response.cancelled","response":{"id":"resp_one"}}`), func([]byte) error { return nil }))
	require.Error(t, r.cancelCurrent([]byte(`{"type":"response.cancel"}`)))
	next := r.currentTurn()
	require.NotSame(t, target, next)
	turnCtx, cancelTurn := context.WithCancelCause(ctx)
	defer cancelTurn(nil)
	r.install(next, nil, cancelTurn)
	require.Error(t, r.cancelCurrent([]byte(`{"type":"response.cancel","response_id":"resp_one"}`)))
	require.NoError(t, turnCtx.Err())
	require.NoError(t, r.cancelCurrent([]byte(`{"type":"response.cancel"}`)))
	require.ErrorIs(t, context.Cause(turnCtx), errOpenAIWSClientTurnCancelled)
	require.NoError(t, ctx.Err(), "turn cancellation must not cancel the sole client reader")
}

func TestOpenAIWSIngressControlIdleCancelCannotReachPendingNextTurn(t *testing.T) {
	old := &openAIWSIngressControlTurn{responseID: "resp_old", active: false}
	r := &openAIWSIngressControlReader{turn: old, frames: make(chan []byte, 1), done: make(chan struct{})}
	r.frames <- []byte(`{"type":"response.create","input":"next"}`)
	require.Error(t, r.cancelCurrent([]byte(`{"type":"response.cancel"}`)))
	_, err := r.readNext(context.Background(), 0)
	require.NoError(t, err)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	next := r.currentTurn()
	r.install(next, nil, cancel)
	require.NoError(t, ctx.Err())
	require.False(t, r.cancelled(next))
}

func TestOpenAIWSIngressControlProjectsAndBoundsCancelPayload(t *testing.T) {
	target := &openAIWSIngressControlTurn{active: true, responseID: "resp_one"}
	r := &openAIWSIngressControlReader{turn: target}
	var sent []byte
	r.install(target, func(payload []byte) error { sent = append([]byte(nil), payload...); return nil }, func(error) {})
	require.Error(t, r.cancelCurrent([]byte(`{"type":"response.cancel","response_id":1}`)))
	require.Error(t, r.cancelCurrent([]byte(`{"type":"response.cancel","response_id":"`+strings.Repeat("x", 257)+`"}`)))
	require.Error(t, r.cancelCurrent([]byte(`{"type":"response.cancel","input":"`+strings.Repeat("x", 4096)+`"}`)))
	require.NoError(t, r.cancelCurrent([]byte(`{"type":"response.cancel","response_id":"resp_one","input":"must-not-forward","account_id":999,"model":"another"}`)))
	require.JSONEq(t, `{"type":"response.cancel","response_id":"resp_one"}`, string(sent))
	r.detach(target)
}

func TestOpenAIWSIngressControlTerminalCompletionRace(t *testing.T) {
	for _, test := range []struct {
		kind, status string
		completed    bool
	}{{"response.completed", "", true}, {"response.completed", "completed", true}, {"response.done", "completed", true}, {"response.done", "cancelled", false}, {"response.done", "incomplete", false}, {"response.done", "", false}, {"response.incomplete", "incomplete", false}} {
		payload := []byte(`{"type":"` + test.kind + `","response":{"status":"` + test.status + `"}}`)
		require.Equal(t, test.completed, openAIWSTerminalCompletedBeforeCancel(test.kind, payload), string(payload))
	}
}

func TestOpenAIWSIngressControlFreshRestartRejectsOldLineage(t *testing.T) {
	for _, payload := range []string{`{"type":"response.create","input":"fresh"}`, `{"type":"response.create","input":[{"role":"user","content":"fresh"}]}`, `{"type":"response.create","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fresh"}]}]}`} {
		require.True(t, openAIWSFreshRequestAfterCancel([]byte(payload)), payload)
	}
	for _, payload := range []string{`{"previous_response_id":"resp_cancelled","input":"continue"}`, `{"input":[{"type":"function_call_output","call_id":"old","output":"done"}]}`, `{"input":[{"type":"tool_search_output","id":"old"}]}`, `{"input":[{"type":"item_reference","id":"old"}]}`, `{"input":[{"type":"function_call","call_id":"old","name":"exec","arguments":"{}"}]}`, `{"input":[{"id":"old"}]}`} {
		require.False(t, openAIWSFreshRequestAfterCancel([]byte(payload)), payload)
	}
}

type ingressControlNativeConn struct{ *stagedPassthroughConn }

func (c *ingressControlNativeConn) WriteJSON(ctx context.Context, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return c.WriteFrame(ctx, coderws.MessageText, b)
}

type ingressControlBlockedWriteConn struct {
	*stagedPassthroughConn
	started   chan struct{}
	cancelled chan struct{}
}

func (c *ingressControlBlockedWriteConn) WriteJSON(ctx context.Context, v any) error {
	close(c.started)
	<-ctx.Done()
	close(c.cancelled)
	return ctx.Err()
}

func TestOpenAIWSIngressControlCancelDuringCreateWriteDoesNotReplayOrBill(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, a, upstream := newIngressControlNativeService()
	blocked := &ingressControlBlockedWriteConn{stagedPassthroughConn: upstream, started: make(chan struct{}), cancelled: make(chan struct{})}
	svc.openaiWSPool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: blocked})
	results := make(chan *OpenAIForwardResult, 1)
	errorsSeen := make(chan error, 1)
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{AfterTurn: func(_ int, r *OpenAIForwardResult, e error) { results <- r; errorsSeen <- e }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = client.CloseNow() }()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("upstream write was not entered")
	}
	writeIngressControlFrame(t, client, `{"type":"response.cancel"}`)
	select {
	case <-blocked.cancelled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancel did not stop blocked create write")
	}
	terminal, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.Equal(t, "response_cancelled", gjson.GetBytes(terminal, "error.code").String())
	require.Nil(t, <-results)
	require.True(t, IsOpenAIWSClientTurnCancelled(<-errorsSeen))
	select {
	case err := <-serverErr:
		require.True(t, IsOpenAIWSClientTurnCancelled(err))
	case <-time.After(time.Second):
		t.Fatal("cancelled write did not finish")
	}
	select {
	case replay := <-upstream.writes:
		t.Fatalf("blocked create was replayed: %s", replay)
	default:
	}
}

func newIngressControlNativeService() (*OpenAIGatewayService, *Account, *stagedPassthroughConn) {
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 10
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 2
	upstream := newStagedPassthroughConn()
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: &ingressControlNativeConn{upstream}})
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
	a := passthroughLifecycleAccount()
	a.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	return svc, a, upstream
}

func writeIngressControlFrame(t *testing.T, conn *coderws.Conn, payload string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(payload)))
}

func TestOpenAIWSIngressControlNativeCancellationPreservesKnownUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, a, upstream := newIngressControlNativeService()
	var before, after atomic.Int64
	type turnOutcome struct {
		result *OpenAIForwardResult
		err    error
	}
	outcomes := make(chan turnOutcome, 1)
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{BeforeTurn: func(int) error { before.Add(1); return nil }, AfterTurn: func(_ int, r *OpenAIForwardResult, e error) { after.Add(1); outcomes <- turnOutcome{r, e} }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = client.CloseNow() }()
	require.Equal(t, "response.create", gjson.GetBytes(requirePassthroughUpstreamWrite(t, upstream, time.Second), "type").String())
	upstream.Send(`{"type":"response.created","response":{"id":"resp_cancel","model":"gpt-5.1"}}`)
	created, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.created", gjson.GetBytes(created, "type").String())
	writeIngressControlFrame(t, client, `{"type":"response.cancel","response_id":"resp_cancel"}`)
	require.Equal(t, "response.cancel", gjson.GetBytes(requirePassthroughUpstreamWrite(t, upstream, 500*time.Millisecond), "type").String())
	upstream.Send(`{"type":"response.cancelled","response":{"id":"resp_cancel","model":"gpt-5.1","status":"cancelled","usage":{"input_tokens":7,"output_tokens":3}}}`)
	terminal, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.cancelled", gjson.GetBytes(terminal, "type").String())
	select {
	case outcome := <-outcomes:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.result)
		require.True(t, outcome.result.ClientCancelled)
		require.Equal(t, 7, outcome.result.Usage.InputTokens)
		require.Equal(t, 3, outcome.result.Usage.OutputTokens)
		require.Equal(t, "ws", outcome.result.TransportDetails.Upstream)
	case <-time.After(time.Second):
		t.Fatal("cancel did not finish the turn")
	}
	select {
	case err := <-serverErr:
		require.True(t, IsOpenAIWSClientTurnCancelled(err))
	case <-time.After(time.Second):
		t.Fatal("cancel did not end the native connection")
	}
	require.EqualValues(t, 1, before.Load())
	require.EqualValues(t, 1, after.Load())
	select {
	case replay := <-upstream.writes:
		t.Fatalf("cancelled input must not replay: %s", replay)
	default:
	}
	select {
	case <-upstream.closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled native socket was reusable")
	}
}

func TestOpenAIWSIngressControlNativeUnconfirmedCancelDoesNotInventUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, a, upstream := newIngressControlNativeService()
	results := make(chan *OpenAIForwardResult, 1)
	errorsSeen := make(chan error, 1)
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{AfterTurn: func(_ int, r *OpenAIForwardResult, e error) { results <- r; errorsSeen <- e }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = client.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	upstream.Send(`{"type":"response.created","response":{"id":"resp_stalled","model":"gpt-5.1"}}`)
	_, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	writeIngressControlFrame(t, client, `{"type":"response.cancel","response_id":"resp_stalled"}`)
	_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	terminal, err := readPassthroughLifecycleFrame(t, client, 4*time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.incomplete", gjson.GetBytes(terminal, "type").String())
	require.False(t, gjson.GetBytes(terminal, "response.usage").Exists())
	select {
	case r := <-results:
		require.Nil(t, r)
	case <-time.After(time.Second):
		t.Fatal("unknown usage produced no outcome")
	}
	require.True(t, IsOpenAIWSClientTurnCancelled(<-errorsSeen))
	select {
	case err := <-serverErr:
		require.True(t, IsOpenAIWSClientTurnCancelled(err))
	case <-time.After(time.Second):
		t.Fatal("unconfirmed cancellation remained open")
	}
}

func TestOpenAIWSIngressControlNativeDoneCancelledDoesNotInventUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, a, upstream := newIngressControlNativeService()
	results := make(chan *OpenAIForwardResult, 1)
	errorsSeen := make(chan error, 1)
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{AfterTurn: func(_ int, r *OpenAIForwardResult, e error) { results <- r; errorsSeen <- e }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = client.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	upstream.Send(`{"type":"response.created","response":{"id":"resp_done_cancel","model":"gpt-5.1"}}`)
	_, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	writeIngressControlFrame(t, client, `{"type":"response.cancel","response_id":"resp_done_cancel"}`)
	_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	upstream.Send(`{"type":"response.done","response":{"id":"resp_done_cancel","status":"cancelled"}}`)
	terminal, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.Equal(t, "cancelled", gjson.GetBytes(terminal, "response.status").String())
	require.Nil(t, <-results)
	require.True(t, IsOpenAIWSClientTurnCancelled(<-errorsSeen))
	select {
	case err := <-serverErr:
		require.True(t, IsOpenAIWSClientTurnCancelled(err))
	case <-time.After(time.Second):
		t.Fatal("cancelled done turn did not finish")
	}
}

func TestOpenAIWSIngressControlNativeCancelPreservesCompletedImageAndObservedModel(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "watchdog", true: "upstream terminal"}[terminal], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			svc, a, upstream := newIngressControlNativeService()
			outcomes := make(chan *OpenAIForwardResult, 1)
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(*gin.Context) *OpenAIWSIngressHooks {
				return &OpenAIWSIngressHooks{AfterTurn: func(_ int, r *OpenAIForwardResult, e error) {
					if e == nil {
						outcomes <- r
					}
				}}
			})
			defer server.Close()
			client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-5.1","stream":true,"service_tier":"priority","input":"draw","tools":[{"type":"image_generation","model":"gpt-image-2","size":"1024x1024"}]}`)
			defer func() { _ = client.CloseNow() }()
			_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
			upstream.Send(`{"type":"response.created","response":{"id":"resp_image_cancel","model":"gpt-5.5","service_tier":"priority"}}`)
			upstream.Send(`{"type":"response.output_item.done","response_id":"resp_image_cancel","item":{"id":"image_done","type":"image_generation_call","status":"completed","result":"final-image"},"usage":{"input_tokens":9,"output_tokens":4}}`)
			for i := 0; i < 2; i++ {
				_, err := readPassthroughLifecycleFrame(t, client, time.Second)
				require.NoError(t, err)
			}
			writeIngressControlFrame(t, client, `{"type":"response.cancel","response_id":"resp_image_cancel"}`)
			_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
			if terminal {
				upstream.Send(`{"type":"response.cancelled","response":{"id":"resp_image_cancel","status":"cancelled","usage":{"input_tokens":9,"output_tokens":4}}}`)
			}
			_, err := readPassthroughLifecycleFrame(t, client, 4*time.Second)
			require.NoError(t, err)
			select {
			case result := <-outcomes:
				require.NotNil(t, result)
				require.True(t, result.ClientCancelled)
				require.Equal(t, 1, result.ImageCount)
				require.Equal(t, "gpt-image-2", result.BillingModel)
				require.NotEmpty(t, result.ImageSize)
				require.NotEmpty(t, result.ImageInputSize)
				require.Equal(t, "gpt-5.5", result.UpstreamResponseModel)
				require.Empty(t, result.UpstreamResponseServiceTier, "a preamble is not a trusted final service-tier declaration")
				require.NotNil(t, result.ServiceTier)
				require.Equal(t, "priority", *result.ServiceTier)
				require.Equal(t, 9, result.Usage.InputTokens)
				require.Equal(t, 4, result.Usage.OutputTokens)
			case <-time.After(time.Second):
				t.Fatal("image cancellation result was lost")
			}
			select {
			case err := <-serverErr:
				require.True(t, IsOpenAIWSClientTurnCancelled(err))
			case <-time.After(time.Second):
				t.Fatal("image cancellation did not finish")
			}
		})
	}
}

type ingressControlHTTPUpstream struct {
	HTTPUpstream
	mu        sync.Mutex
	calls     int
	known     bool
	cancelled chan struct{}
	bodies    [][]byte
	headers   []http.Header
}

func (u *ingressControlHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	u.mu.Lock()
	u.calls++
	call := u.calls
	u.bodies = append(u.bodies, b)
	u.headers = append(u.headers, cloneHeader(req.Header))
	u.mu.Unlock()
	if call > 1 {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fresh\",\"model\":\"gpt-5.1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"))}, nil
	}
	reader, writer := io.Pipe()
	go func() {
		defer func() { _ = writer.Close() }()
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_http_cancel\",\"model\":\"gpt-5.1\"}}\n\n")
		delta := `{"type":"response.output_text.delta","response_id":"resp_http_cancel","delta":"partial"}`
		if u.known {
			delta = `{"type":"response.output_text.delta","response_id":"resp_http_cancel","delta":"partial","usage":{"input_tokens":7,"output_tokens":3}}`
		}
		_, _ = io.WriteString(writer, "data: "+delta+"\n\n")
		<-req.Context().Done()
		close(u.cancelled)
		_ = writer.CloseWithError(req.Context().Err())
	}()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
}

func TestOpenAIWSIngressControlHTTPBridgeCancellationAndFreshContinuation(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown usage", true: "known usage"}[known], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
			upstream := &ingressControlHTTPUpstream{known: known, cancelled: make(chan struct{})}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
			a := passthroughLifecycleAccount()
			a.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeHTTPBridge
			type outcome struct {
				result *OpenAIForwardResult
				err    error
			}
			outcomes := make(chan outcome, 3)
			var before, after atomic.Int64
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(*gin.Context) *OpenAIWSIngressHooks {
				return &OpenAIWSIngressHooks{BeforeTurn: func(int) error { before.Add(1); return nil }, AfterTurn: func(_ int, r *OpenAIForwardResult, e error) { after.Add(1); outcomes <- outcome{r, e} }}
			})
			defer server.Close()
			client := dialPassthroughLifecycleClient(t, server)
			defer func() { _ = client.CloseNow() }()
			for i := 0; i < 2; i++ {
				_, err := readPassthroughLifecycleFrame(t, client, time.Second)
				require.NoError(t, err)
			}
			// Preserve the legacy one-frame pipeline without admitting it as the
			// active turn: the following cancel must still target resp_http_cancel.
			writeIngressControlFrame(t, client, `{"type":"response.create","model":"gpt-5.1","input":"fresh","stream":true}`)
			writeIngressControlFrame(t, client, `{"type":"response.cancel","response_id":"resp_http_cancel"}`)
			select {
			case <-upstream.cancelled:
			case <-time.After(time.Second):
				t.Fatal("HTTP request was not cancelled promptly")
			}
			terminal, err := readPassthroughLifecycleFrame(t, client, time.Second)
			require.NoError(t, err)
			require.Equal(t, "response.incomplete", gjson.GetBytes(terminal, "type").String())
			require.Equal(t, "interrupted", gjson.GetBytes(terminal, "response.incomplete_details.reason").String())
			require.Equal(t, known, gjson.GetBytes(terminal, "response.usage").Exists())
			first := <-outcomes
			if known {
				require.NoError(t, first.err)
				require.True(t, first.result.ClientCancelled)
				require.Equal(t, 7, first.result.Usage.InputTokens)
				require.Equal(t, 3, first.result.Usage.OutputTokens)
			} else {
				require.Nil(t, first.result)
				require.True(t, IsOpenAIWSClientTurnCancelled(first.err))
			}
			completed, err := readPassthroughLifecycleFrame(t, client, time.Second)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(completed, "type").String())
			second := <-outcomes
			require.NoError(t, second.err)
			require.Equal(t, "resp_fresh", second.result.RequestID)
			require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
			select {
			case err := <-serverErr:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("bridge did not close")
			}
			require.EqualValues(t, 2, before.Load())
			require.EqualValues(t, 2, after.Load())
			upstream.mu.Lock()
			require.Equal(t, 2, upstream.calls)
			require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
			require.Empty(t, upstream.headers[1].Get(openAIWSTurnStateHeader))
			require.Equal(t, "fresh", gjson.GetBytes(upstream.bodies[1], "input").String())
			upstream.mu.Unlock()
		})
	}
}

func TestOpenAIWSIngressControlHTTPRejectsQueuedOldContinuationAfterCancel(t *testing.T) {
	for _, next := range []string{`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_http_cancel","input":"continue"}`, `{"type":"response.create","model":"gpt-5.1","input":[{"type":"item_reference","id":"old_item"}]}`, `{"type":"response.create","model":"gpt-5.1","input":[{"type":"function_call_output","call_id":"old_call","output":"already executed"}]}`} {
		gin.SetMode(gin.TestMode)
		cfg := passthroughLifecycleConfig()
		cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 5
		upstream := &ingressControlHTTPUpstream{cancelled: make(chan struct{})}
		svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
		a := passthroughLifecycleAccount()
		a.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeHTTPBridge
		var before, after atomic.Int64
		server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(c *gin.Context) *OpenAIWSIngressHooks {
			c.Request.Header.Set(openAIWSTurnStateHeader, "cancelled-turn-state")
			return &OpenAIWSIngressHooks{BeforeTurn: func(int) error { before.Add(1); return nil }, AfterTurn: func(int, *OpenAIForwardResult, error) { after.Add(1) }}
		})
		client := dialPassthroughLifecycleClient(t, server)
		for i := 0; i < 2; i++ {
			_, err := readPassthroughLifecycleFrame(t, client, time.Second)
			require.NoError(t, err)
		}
		writeIngressControlFrame(t, client, next)
		writeIngressControlFrame(t, client, `{"type":"response.cancel","response_id":"resp_http_cancel"}`)
		terminal, err := readPassthroughLifecycleFrame(t, client, time.Second)
		require.NoError(t, err)
		require.Equal(t, "response.incomplete", gjson.GetBytes(terminal, "type").String())
		select {
		case err := <-serverErr:
			var closeErr *OpenAIWSClientCloseError
			require.ErrorAs(t, err, &closeErr)
			require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
			require.Contains(t, closeErr.Reason(), "message-only")
		case <-time.After(time.Second):
			t.Fatal("old continuation was not rejected")
		}
		upstream.mu.Lock()
		require.Equal(t, 1, upstream.calls, "cancelled tool/input lineage must not be replayed")
		upstream.mu.Unlock()
		require.EqualValues(t, 1, before.Load())
		require.EqualValues(t, 1, after.Load())
		_ = client.CloseNow()
		server.Close()
	}
}

func TestOpenAIWSIngressControlPipelinesOneCreateAfterTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, a, upstream := newIngressControlNativeService()
	contexts := make(chan *gin.Context, 1)
	var before, after atomic.Int64
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(c *gin.Context) *OpenAIWSIngressHooks {
		contexts <- c
		return &OpenAIWSIngressHooks{BeforeTurn: func(int) error { before.Add(1); return nil }, AfterTurn: func(int, *OpenAIForwardResult, error) { after.Add(1) }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = client.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	c := <-contexts
	upstream.Send(`{"type":"response.created","response":{"id":"resp_pipeline_one","model":"gpt-5.1"}}`)
	_, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	writeIngressControlFrame(t, client, `{"type":"response.create","model":"gpt-5.1","input":"second","stream":false}`)
	readerValue, _ := c.Get(openAIWSIngressReaderContextKey)
	reader, ok := readerValue.(*openAIWSIngressControlReader)
	require.True(t, ok)
	require.Eventually(t, func() bool { return len(reader.frames) == 1 }, time.Second, time.Millisecond)
	select {
	case early := <-upstream.writes:
		t.Fatalf("pipelined request ran before terminal: %s", early)
	default:
	}
	writeIngressControlFrame(t, client, `{"type":"response.create","model":"gpt-5.1","input":"too many","stream":false}`)
	rejected, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.Equal(t, "invalid_control_frame", gjson.GetBytes(rejected, "error.code").String())
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_pipeline_one","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	_, err = readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	second := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, "second", gjson.GetBytes(second, "input").String())
	require.EqualValues(t, 1, after.Load())
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_pipeline_two","model":"gpt-5.1","usage":{"input_tokens":2,"output_tokens":1}}}`)
	_, err = readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("pipeline did not close")
	}
	require.EqualValues(t, 2, before.Load())
	require.EqualValues(t, 2, after.Load())
	select {
	case extra := <-upstream.writes:
		t.Fatalf("excess queued request reached upstream: %s", extra)
	default:
	}
}

func TestOpenAIWSIngressControlCancelDoesNotReplayQueuedNativeCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, a, upstream := newIngressControlNativeService()
	var turns atomic.Int64
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, a, func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{AfterTurn: func(int, *OpenAIForwardResult, error) { turns.Add(1) }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = client.CloseNow() }()
	_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	upstream.Send(`{"type":"response.created","response":{"id":"resp_queued_cancel","model":"gpt-5.1"}}`)
	_, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	writeIngressControlFrame(t, client, `{"type":"response.create","model":"gpt-5.1","input":"queued","stream":false}`)
	writeIngressControlFrame(t, client, `{"type":"response.cancel","response_id":"resp_queued_cancel"}`)
	control := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, "response.cancel", gjson.GetBytes(control, "type").String())
	require.Equal(t, "resp_queued_cancel", gjson.GetBytes(control, "response_id").String())
	upstream.Send(`{"type":"response.cancelled","response":{"id":"resp_queued_cancel","status":"cancelled","usage":{"input_tokens":1,"output_tokens":1}}}`)
	_, err = readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	select {
	case err := <-serverErr:
		require.True(t, IsOpenAIWSClientTurnCancelled(err))
	case <-time.After(time.Second):
		t.Fatal("queued cancel did not finish")
	}
	require.EqualValues(t, 1, turns.Load())
	select {
	case replay := <-upstream.writes:
		t.Fatalf("queued native create was replayed after cancel: %s", replay)
	default:
	}
}

func TestOpenAIWSIngressControlHTTPBufferedCancelledTerminalDoesNotInventUsage(t *testing.T) {
	for _, terminal := range []string{`{"type":"response.done","response":{"id":"resp_buffered","status":"cancelled"}}`, `{"type":"response.incomplete","response":{"id":"resp_buffered","status":"incomplete","incomplete_details":{"reason":"interrupted"}}}`, `{"type":"response.completed","response":{"id":"resp_buffered","status":"completed"}}`} {
		ctx, cancel := context.WithCancelCause(context.Background())
		target := &openAIWSIngressControlTurn{active: true}
		reader := &openAIWSIngressControlReader{ctx: context.Background(), turn: target}
		reader.install(target, nil, cancel)
		turnCtx := withOpenAIWSIngressControl(ctx, reader, target)
		response := `data: {"type":"response.created","response":{"id":"resp_buffered","model":"gpt-5.1"}}` + "\n\n" + "data: " + terminal + "\n\n"
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response))}}
		svc := &OpenAIGatewayService{cfg: passthroughLifecycleConfig(), httpUpstream: upstream}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		payload := []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":"hi"}`)
		result, err := svc.proxyOpenAIWSHTTPBridgeTurn(turnCtx, c, passthroughLifecycleAccount(), "sk-test", payload, len(payload), "gpt-5.1", "", "", "", "", 1, func(message []byte) error {
			return reader.writeTurn(target, message, func(message []byte) error {
				if gjson.GetBytes(message, "type").String() == "response.created" {
					return reader.cancelCurrent([]byte(`{"type":"response.cancel","response_id":"resp_buffered"}`))
				}
				return nil
			})
		})
		require.True(t, IsOpenAIWSClientTurnCancelled(err), "%s: %v", terminal, err)
		require.Nil(t, result, "unknown usage must not become a successful zero-token record")
		cancel(nil)
	}
}

func TestOpenAIWSIngressControlCachedReaderCannotBecomePassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, a, _ := newIngressControlNativeService()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &openAIWSIngressControlReader{ctx: ctx, turn: &openAIWSIngressControlTurn{active: true}}
	c, _ := gin.CreateTestContext(nil)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	// The pointer is used only for identity before any I/O in this regression.
	conn := &coderws.Conn{}
	r.conn = conn
	c.Set(openAIWSIngressReaderContextKey, r)
	a.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
	err := svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, a, "sk-test", []byte(`{"type":"response.create","model":"gpt-5.1"}`), nil)
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusTryAgainLater, closeErr.StatusCode())
	require.Contains(t, closeErr.Reason(), "mode changed")
	require.False(t, errors.Is(err, errOpenAIWSClientTurnCancelled))
}
