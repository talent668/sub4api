package service

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const wsFallbackTestWSBody = `{"type":"response.create","model":"gpt-5.1","input":"hello","store":false}`
const wsFallbackTestHTTPBody = `{"store":false,"input":"hello","model":"gpt-5.1","stream":true}`

type fallbackTestEntry struct{ binding, attempt string }

type fallbackPartialWriteConn struct{ *ingressControlNativeConn }

func (c *fallbackPartialWriteConn) WriteJSON(context.Context, any) error {
	return io.ErrUnexpectedEOF // Delivery is uncertain even without any output.
}

type fallbackPartialThenFailedDialer struct {
	first openAIWSClientConn
	calls atomic.Int32
}

func (d *fallbackPartialThenFailedDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	if d.calls.Add(1) == 1 {
		return d.first, 0, nil, nil
	}
	return nil, 0, nil, errors.New("subsequent websocket dial failed")
}

type fallbackTestStore struct {
	stubGatewayCache
	mu          sync.Mutex
	entries     map[string]fallbackTestEntry
	unavailable bool
	putStarted  chan struct{}
	putRelease  chan struct{}
}

func (s *fallbackTestStore) PutWSFallbackReceipt(ctx context.Context, apiKeyID int64, hash, binding, attempt string) (bool, error) {
	if s.putStarted != nil {
		close(s.putStarted)
		select {
		case <-s.putRelease:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return false, errors.New("observation storage unavailable")
	}
	if s.entries == nil {
		s.entries = map[string]fallbackTestEntry{}
	}
	s.entries[fmt.Sprintf("%d:%s", apiKeyID, hash)] = fallbackTestEntry{binding, attempt}
	return true, nil
}

func (s *fallbackTestStore) ConsumeWSFallbackReceipt(_ context.Context, apiKeyID int64, hash, binding string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable {
		return "", errors.New("observation storage unavailable")
	}
	key := fmt.Sprintf("%d:%s", apiKeyID, hash)
	entry, exists := s.entries[key]
	if !exists || entry.binding != binding {
		return "", nil
	}
	delete(s.entries, key)
	return entry.attempt, nil
}

func fallbackTestContext(t *testing.T, owner [3]int64) *gin.Context {
	t.Helper()
	c, _ := newFastWSBridgeTestGinContext(t)
	group := owner[2]
	c.Set("api_key", &APIKey{ID: owner[1], UserID: owner[0], GroupID: &group})
	c.Request.Header.Set("session-id", "shared-session-does-not-prove-fallback")
	return c
}

func prepareFallbackTestReceipt(t *testing.T, store *fallbackTestStore, owner [3]int64) (string, string) {
	t.Helper()
	receipt := "wfc1_" + strings.Repeat("A", 43)
	hash, ok := wsFallbackReceiptParts(receipt)
	require.True(t, ok)
	digest, ok := wsFallbackRequestDigest(fallbackTestContext(t, owner), []byte(wsFallbackTestWSBody), true)
	require.True(t, ok)
	id := uuid.NewString()
	stored, err := store.PutWSFallbackReceipt(context.Background(), owner[1], hash, wsFallbackBinding(receipt, owner, digest), id)
	require.NoError(t, err)
	require.True(t, stored)
	return receipt, id
}

func TestWSClientFallbackPayloadIsBoundedAndUnambiguous(t *testing.T) {
	wsDigest, ok := wsFallbackPayloadDigest([]byte(wsFallbackTestWSBody), true)
	require.True(t, ok)
	httpDigest, ok := wsFallbackPayloadDigest([]byte(wsFallbackTestHTTPBody), false)
	require.True(t, ok)
	require.Equal(t, wsDigest, httpDigest)
	for _, body := range []string{
		`{"type":"response.create","model":"a","model":"b","input":"hello"}`,
		`{"type":"response.create","model":"a","input":[{"role":"user","role":"assistant","content":"hello"}]}`,
		`{"type":"response.create","model":"a","previous_response_id":"resp_old","input":"hello"}`,
		`{"type":"response.create","model":"a","input":[{"type":"function_call_output","call_id":"old","output":"done"}]}`,
		`{"type":"response.create","model":"a","input":"` + strings.Repeat("x", 256*1024) + `"}`,
		`{"type":"response.create","model":"a","input":"hello","nested":` + strings.Repeat("[", 34) + `0` + strings.Repeat("]", 34) + `}`,
	} {
		_, ok := wsFallbackPayloadDigest([]byte(body), true)
		require.False(t, ok)
	}
	_, ok = wsFallbackReceiptParts("wfc1_" + strings.Repeat("A", 1000))
	require.False(t, ok)
}

func TestWSClientFallbackScopePayloadAndReplay(t *testing.T) {
	owner := [3]int64{10, 20, 30}
	for _, wrongOwner := range [][3]int64{{11, 20, 30}, {10, 21, 30}, {10, 20, 31}} {
		store := &fallbackTestStore{}
		receipt, id := prepareFallbackTestReceipt(t, store, owner)
		svc := &OpenAIGatewayService{cache: store}
		wrong := fallbackTestContext(t, wrongOwner)
		wrong.Request.Header.Set(WSFallbackReceiptHeader, receipt)
		svc.ObserveClientWSFallback(wrong, []byte(wsFallbackTestHTTPBody))
		_, linked := wrong.Get(wsFallbackVerifiedKey)
		require.False(t, linked)
		correct := fallbackTestContext(t, owner)
		correct.Request.Header.Set(WSFallbackReceiptHeader, receipt)
		svc.ObserveClientWSFallback(correct, []byte(wsFallbackTestHTTPBody))
		verified, exists := correct.Get(wsFallbackVerifiedKey)
		require.True(t, exists, "a wrong principal must not burn the owner's receipt")
		require.Equal(t, &UsageClientWSFailure{AttemptID: id, Evidence: "receipt_v1"}, verified)
		replayed := fallbackTestContext(t, owner)
		replayed.Request.Header.Set(WSFallbackReceiptHeader, receipt)
		svc.ObserveClientWSFallback(replayed, []byte(wsFallbackTestHTTPBody))
		_, linked = replayed.Get(wsFallbackVerifiedKey)
		require.False(t, linked)
	}
	store := &fallbackTestStore{}
	receipt, _ := prepareFallbackTestReceipt(t, store, owner)
	svc := &OpenAIGatewayService{cache: store}
	wrongBody := fallbackTestContext(t, owner)
	wrongBody.Request.Header.Set(WSFallbackReceiptHeader, receipt)
	svc.ObserveClientWSFallback(wrongBody, []byte(strings.ReplaceAll(wsFallbackTestHTTPBody, "hello", "different")))
	_, linked := wrongBody.Get(wsFallbackVerifiedKey)
	require.False(t, linked)
	noReceipt := fallbackTestContext(t, owner)
	svc.ObserveClientWSFallback(noReceipt, []byte(wsFallbackTestHTTPBody))
	_, linked = noReceipt.Get(wsFallbackVerifiedKey)
	require.False(t, linked, "same key, session, model, body and timing are insufficient")
	otherConversation := fallbackTestContext(t, owner)
	otherConversation.Request.Header.Set("session-id", "another-session")
	otherConversation.Request.Header.Set(WSFallbackReceiptHeader, receipt)
	svc.ObserveClientWSFallback(otherConversation, []byte(wsFallbackTestHTTPBody))
	_, linked = otherConversation.Get(wsFallbackVerifiedKey)
	require.False(t, linked, "a receipt must not cross conversations sharing an API key")
	correct := fallbackTestContext(t, owner)
	correct.Request.Header.Set(WSFallbackReceiptHeader, receipt)
	svc.ObserveClientWSFallback(correct, []byte(wsFallbackTestHTTPBody))
	_, linked = correct.Get(wsFallbackVerifiedKey)
	require.True(t, linked)
}

func TestWSClientFallbackUnavailableOrMalformedEvidenceDoesNotAffectHTTP(t *testing.T) {
	for _, mode := range []string{"none", "malformed", "duplicate", "unavailable", "valid"} {
		t.Run(mode, func(t *testing.T) {
			store := &fallbackTestStore{}
			owner := [3]int64{10, 20, 30}
			receipt, id := prepareFallbackTestReceipt(t, store, owner)
			svc, upstream, dialer := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), newAnthropicMessagesBridgeCaptureConn(t))
			useAnthropicMessagesBridgeSSEUpstream(upstream)
			svc.cache = store
			account := fastWSBridgeTestAccount(true, false)
			account.Extra[OpenAINativeWSPreferredExtraKey] = true
			c := fallbackTestContext(t, owner)
			if mode != "none" {
				c.Request.Header.Set(WSFallbackReceiptHeader, receipt)
			}
			if mode == "malformed" {
				c.Request.Header.Set(WSFallbackReceiptHeader, "unknown-token")
			}
			if mode == "duplicate" {
				c.Request.Header.Add(WSFallbackReceiptHeader, receipt)
			}
			if mode == "unavailable" {
				store.unavailable = true
			}
			c.Request.Header.Set(WSFallbackOptInHeader, "receipt-v1")
			svc.ObserveClientWSFallback(c, []byte(wsFallbackTestHTTPBody))
			result, err := svc.Forward(context.Background(), c, account, []byte(wsFallbackTestHTTPBody))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Zero(t, dialer.DialCount())
			require.NotNil(t, upstream.lastReq)
			require.Empty(t, upstream.lastReq.Header.Get(WSFallbackReceiptHeader))
			require.Empty(t, upstream.lastReq.Header.Get(WSFallbackOptInHeader))
			require.Equal(t, "http", result.TransportDetails.Client)
			require.Equal(t, "http", result.TransportDetails.Upstream)
			require.Empty(t, result.TransportDetails.FallbackReason)
			if mode == "valid" {
				require.Equal(t, &UsageClientWSFailure{AttemptID: id, Evidence: "receipt_v1"}, result.TransportDetails.ClientWSFailure)
			} else {
				require.Nil(t, result.TransportDetails.ClientWSFailure)
			}
		})
	}
}

func TestWSClientFallbackAllSupportedIdentityHeadersAreBound(t *testing.T) {
	headers := append([]string{"conversation-id", "thread-id", "x-codex-window-id", "x-codex-turn-metadata", "x-openai-subagent"}, explicitOpenAIHeaderSessionNames...)
	for _, header := range headers {
		t.Run(header, func(t *testing.T) {
			owner := [3]int64{10, 20, 30}
			store := &fallbackTestStore{}
			svc := &OpenAIGatewayService{cache: store}
			receipt := "wfc1_" + strings.Repeat("A", 43)
			hash, ok := wsFallbackReceiptParts(receipt)
			require.True(t, ok)
			original := fallbackTestContext(t, owner)
			original.Request.Header.Del("session-id")
			original.Request.Header.Set(header, "original-conversation")
			digest, ok := wsFallbackRequestDigest(original, []byte(wsFallbackTestWSBody), true)
			require.True(t, ok)
			_, err := store.PutWSFallbackReceipt(context.Background(), owner[1], hash, wsFallbackBinding(receipt, owner, digest), uuid.NewString())
			require.NoError(t, err)
			wrong := fallbackTestContext(t, owner)
			wrong.Request.Header.Del("session-id")
			wrong.Request.Header.Set(header, "different-conversation")
			wrong.Request.Header.Set(WSFallbackReceiptHeader, receipt)
			svc.ObserveClientWSFallback(wrong, []byte(wsFallbackTestHTTPBody))
			_, linked := wrong.Get(wsFallbackVerifiedKey)
			require.False(t, linked)
			original.Request.Header.Set(WSFallbackReceiptHeader, receipt)
			svc.ObserveClientWSFallback(original, []byte(wsFallbackTestHTTPBody))
			_, linked = original.Get(wsFallbackVerifiedKey)
			require.True(t, linked, "wrong identity must not consume the correct receipt")
		})
	}
}

func TestWSClientFallbackSubmissionAndCancellationAreMonotonicAcrossAttempts(t *testing.T) {
	svc, _ := nativeWSTestServiceAccount()
	c := fallbackTestContext(t, [3]int64{10, 20, 30})
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)
	c.Request.Header.Set(WSFallbackOptInHeader, "receipt-v1")
	svc.BeginWSFallbackCorrelation(c, []byte(wsFallbackTestWSBody))
	ctx := withWSFallbackObservation(context.Background(), c)
	markWSFallbackSubmitted(ctx)
	svc.BeginWSFallbackCorrelation(c, []byte(wsFallbackTestWSBody))
	ctx = withWSFallbackObservation(context.Background(), c)
	markWSFallbackAcquireFailed(ctx)
	value, _ := c.Get(wsFallbackObservationKey)
	observation, ok := value.(*wsFallbackObservation)
	require.True(t, ok)
	require.Equal(t, wsFallbackSubmitted, observation.phase.Load(), "a later dial failure must not erase an earlier partial write")
	reader := &openAIWSIngressControlReader{ctx: ctx, turn: &openAIWSIngressControlTurn{active: true}}
	require.NoError(t, reader.cancelCurrent([]byte(`{"type":"response.cancel"}`)))
	require.Equal(t, wsFallbackSubmitted, observation.phase.Load(), "cancellation must not make a submitted request eligible again")
	fresh := fallbackTestContext(t, [3]int64{10, 20, 30})
	_, exists := fresh.Get(wsFallbackObservationKey)
	require.False(t, exists)
}

func TestWSClientFallbackOptInFailedDialReceiptAndHTTPAssociation(t *testing.T) {
	for _, variant := range []string{"confirmed", "no_opt_in", "partial_submission", "partial_write_then_dial_failure", "cancelled", "store_down", "cancel_during_store", "submit_during_store"} {
		t.Run(variant, func(t *testing.T) {
			store := &fallbackTestStore{unavailable: variant == "store_down"}
			if variant == "cancel_during_store" || variant == "submit_during_store" {
				store.putStarted, store.putRelease = make(chan struct{}), make(chan struct{})
			}
			svc, account, upstream := newIngressControlNativeService()
			defer svc.openaiWSPool.Close()
			svc.cache = store
			svc.openaiWSPool.setClientDialerForTest(&failingFastWSBridgeDialer{})
			if variant == "partial_write_then_dial_failure" {
				svc.openaiWSPool.setClientDialerForTest(&fallbackPartialThenFailedDialer{first: &fallbackPartialWriteConn{&ingressControlNativeConn{upstream}}})
			}
			owner := [3]int64{10, 20, 30}
			done := make(chan error, 1)
			observedContext := make(chan context.Context, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_, body, err := conn.Read(r.Context())
				if err != nil {
					done <- err
					return
				}
				c := fallbackTestContext(t, owner)
				c.Request = r
				SetOpenAIClientTransport(c, OpenAIClientTransportWS)
				svc.BeginWSFallbackCorrelation(c, body)
				ctx := withWSFallbackObservation(r.Context(), c)
				observedContext <- ctx
				if variant == "partial_submission" {
					markWSFallbackSubmitted(ctx)
				}
				if variant == "cancelled" {
					markWSFallbackCancelled(ctx)
				}
				err = svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "sk-test", body, nil)
				if variant == "partial_write_then_dial_failure" {
					value, _ := c.Get(wsFallbackObservationKey)
					observation, ok := value.(*wsFallbackObservation)
					if !ok || observation.phase.Load() != wsFallbackSubmitted {
						t.Error("actual upstream write did not permanently block receipt issuance")
						done <- errors.New("partial write was not marked before submission")
						return
					}
					if !observation.acquireFailed.Load() {
						err = svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "sk-test", body, nil)
					}
					if !observation.acquireFailed.Load() {
						t.Error("test did not reach an actual dial failure after the partial write")
						done <- errors.New("cross-attempt dial failure was not observed")
						return
					}
				}
				svc.WriteWSFallbackReceipt(c, conn, err)
				_ = conn.Close(coderws.StatusInternalError, "upstream websocket unavailable")
				done <- err
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			headers := http.Header{}
			headers.Set("session-id", "shared-session-does-not-prove-fallback")
			if variant != "no_opt_in" {
				headers.Set(WSFallbackOptInHeader, "receipt-v1")
			}
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: headers})
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(wsFallbackTestWSBody)))
			if store.putStarted != nil {
				select {
				case <-store.putStarted:
					observed := <-observedContext
					if variant == "cancel_during_store" {
						markWSFallbackCancelled(observed)
					} else {
						markWSFallbackSubmitted(observed)
					}
					close(store.putRelease)
				case <-ctx.Done():
					t.Fatal("receipt storage was not entered")
				}
			}
			_, frame, err := client.Read(ctx)
			if variant == "confirmed" {
				require.NoError(t, err)
				receipt := gjson.GetBytes(frame, "sub4api_fallback.receipt").String()
				id := gjson.GetBytes(frame, "sub4api_fallback.ws_attempt_id").String()
				require.Len(t, receipt, 48)
				httpContext := fallbackTestContext(t, owner)
				httpContext.Request.Header.Set(WSFallbackReceiptHeader, receipt)
				svc.ObserveClientWSFallback(httpContext, []byte(wsFallbackTestHTTPBody))
				traceCtx, trace := withUsageTransportTrace(context.Background(), httpContext)
				observeUsageTransport(traceCtx, "http", "")
				result := trace.finish(&OpenAIForwardResult{})
				require.Equal(t, &UsageClientWSFailure{AttemptID: id, Evidence: "receipt_v1"}, result.TransportDetails.ClientWSFailure)
				require.NotContains(t, UsageTransportDetailsJSON(result.TransportDetails), receipt)
				_, _, closeErr := client.Read(ctx)
				require.Equal(t, coderws.StatusInternalError, coderws.CloseStatus(closeErr))
			} else {
				require.Error(t, err, "uncertain requests must not receive a receipt")
			}
			select {
			case err := <-done:
				require.Error(t, err)
			case <-ctx.Done():
				t.Fatal("proxy did not terminate")
			}
			if store.putStarted != nil {
				store.mu.Lock()
				count := len(store.entries)
				store.mu.Unlock()
				require.Equal(t, 1, count, "the race must block issuance after a successful Put, not merely time out storage")
			}
		})
	}
}

func TestWSClientFallbackJSONProjectsOnlySafeVerifiedFields(t *testing.T) {
	id := uuid.NewString()
	value := ParseUsageTransportDetailsJSON(`{"client":"http","upstream":"http","client_ws_failure":{"attempt_id":"` + id + `","evidence":"receipt_v1","receipt":"secret"}}`)
	require.Equal(t, &UsageClientWSFailure{AttemptID: id, Evidence: "receipt_v1"}, value.ClientWSFailure)
	require.NotContains(t, UsageTransportDetailsJSON(value), "secret")
	value.Client = "ws"
	require.Nil(t, NormalizeUsageTransportDetails(value).ClientWSFailure)
	value.Client = "http"
	value.ClientWSFailure.Evidence = "client_claim"
	require.Nil(t, NormalizeUsageTransportDetails(value).ClientWSFailure)
}
