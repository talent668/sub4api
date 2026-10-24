package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	WSFallbackOptInHeader    = "X-Sub4api-WS-Fallback-Correlation"
	WSFallbackReceiptHeader  = "X-Sub4api-WS-Fallback-Receipt"
	WSFallbackReceiptTTL     = 120 * time.Second
	wsFallbackObservationKey = "sub4api_ws_fallback_observation"
	wsFallbackVerifiedKey    = "sub4api_ws_fallback_verified"
	wsFallbackConsumedKey    = "sub4api_ws_fallback_consumed"
)

// WSClientFallbackStore is optional observability storage, never an authority
// for authentication, routing, admission, retries, or billing.
type WSClientFallbackStore interface {
	PutWSFallbackReceipt(context.Context, int64, string, string, string) (bool, error)
	ConsumeWSFallbackReceipt(context.Context, int64, string, string) (string, error)
}

type UsageClientWSFailure struct {
	AttemptID string `json:"attempt_id"`
	Evidence  string `json:"evidence"`
}

func normalizeUsageClientWSFailure(value *UsageClientWSFailure) *UsageClientWSFailure {
	if value == nil || value.Evidence != "receipt_v1" || len(value.AttemptID) != 36 {
		return nil
	}
	id, err := uuid.Parse(value.AttemptID)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 {
		return nil
	}
	return &UsageClientWSFailure{AttemptID: id.String(), Evidence: "receipt_v1"}
}

type wsFallbackObservationContextKey struct{}

const (
	wsFallbackObserving uint32 = iota
	wsFallbackSubmitted
	wsFallbackCancelled
	wsFallbackPublishing
	wsFallbackIssued
)

type wsFallbackObservation struct {
	digest        [32]byte
	owner         [3]int64 // authenticated user, API key, group; never raw credentials
	acquireFailed atomic.Bool
	phase         atomic.Uint32
}

func wsFallbackOwner(c *gin.Context) ([3]int64, bool) {
	if c == nil {
		return [3]int64{}, false
	}
	value, _ := c.Get("api_key")
	key, ok := value.(*APIKey)
	if !ok || key == nil || key.ID <= 0 || key.UserID <= 0 || key.GroupID == nil || *key.GroupID <= 0 {
		return [3]int64{}, false
	}
	return [3]int64{key.UserID, key.ID, *key.GroupID}, true
}

// Bound canonicalization to small, self-contained first turns. Ambiguous JSON,
// duplicate keys (including nested keys), continuations and large bodies remain
// unassociated instead of consuming memory or guessing an identity.
func wsFallbackPayloadDigest(body []byte, websocket bool) ([32]byte, bool) {
	if len(body) == 0 || len(body) > 256*1024 || !utf8.Valid(body) || !gjson.ValidBytes(body) {
		return [32]byte{}, false
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() || !wsFallbackUniqueKeys(root, 0) || !openAIWSFreshRequestAfterCancel(body) {
		return [32]byte{}, false
	}
	if websocket && root.Get("type").String() != "response.create" {
		return [32]byte{}, false
	}
	if model := root.Get("model"); model.Type != gjson.String || model.String() == "" {
		return [32]byte{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return [32]byte{}, false
	}
	if kind, exists := fields["type"]; exists {
		value := gjson.ParseBytes(kind)
		if value.Type != gjson.String || value.String() != "response.create" {
			return [32]byte{}, false
		}
	}
	delete(fields, "type")
	delete(fields, "stream") // WS framing replaces HTTP's streaming flag.
	canonical, err := json.Marshal(fields)
	if err != nil {
		return [32]byte{}, false
	}
	return sha256.Sum256(canonical), true
}

func wsFallbackUniqueKeys(value gjson.Result, depth int) bool {
	if depth > 32 {
		return false
	}
	seen := map[string]struct{}{}
	valid := true
	value.ForEach(func(key, child gjson.Result) bool {
		if value.IsObject() {
			if _, exists := seen[key.String()]; exists {
				valid = false
				return false
			}
			seen[key.String()] = struct{}{}
		}
		if (child.IsObject() || child.IsArray()) && !wsFallbackUniqueKeys(child, depth+1) {
			valid = false
		}
		return valid
	})
	return valid
}

func wsFallbackIdentityHeaders() []string {
	names := make([]string, 0, len(explicitOpenAIHeaderSessionNames)+5)
	names = append(names, explicitOpenAIHeaderSessionNames...)
	return append(names, "conversation-id", openAIWSThreadIDHeader, openAIWSWindowIDHeader, openAIWSTurnMetadataHeader, openAISubagentHeader)
}

func wsFallbackRequestDigest(c *gin.Context, body []byte, websocket bool) ([32]byte, bool) {
	digest, valid := wsFallbackPayloadDigest(body, websocket)
	if !valid || c == nil || c.Request == nil {
		return [32]byte{}, false
	}
	// Session/thread IDs are not proof by themselves, but a receipt must not
	// move between two conversations sharing an API key and identical input.
	hash := sha256.New()
	for _, name := range wsFallbackIdentityHeaders() {
		values := c.Request.Header.Values(name)
		if len(values) > 1 || (len(values) == 1 && len(values[0]) > 512) {
			return [32]byte{}, false
		}
		value := ""
		if len(values) == 1 {
			value = values[0]
		}
		_, _ = fmt.Fprintf(hash, "%s:%d:%s;", name, len(value), value)
	}
	_, _ = hash.Write(digest[:])
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result, true
}

func wsFallbackReceiptParts(receipt string) (string, bool) {
	if len(receipt) != 48 || receipt[:5] != "wfc1_" {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(receipt[5:])
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != receipt[5:] {
		return "", false
	}
	hash := sha256.Sum256([]byte(receipt))
	return hex.EncodeToString(hash[:]), true
}

func wsFallbackBinding(receipt string, owner [3]int64, digest [32]byte) string {
	mac := hmac.New(sha256.New, []byte(receipt))
	_, _ = fmt.Fprintf(mac, "sub4api:ws-fallback:v1:%d:%d:%d:", owner[0], owner[1], owner[2])
	_, _ = mac.Write(digest[:])
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *OpenAIGatewayService) BeginWSFallbackCorrelation(c *gin.Context, body []byte) {
	if s == nil || c == nil || c.Request == nil || s.cfg == nil || s.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled || GetOpenAIClientTransport(c) != OpenAIClientTransportWS {
		return
	}
	values := c.Request.Header.Values(WSFallbackOptInHeader)
	if len(values) != 1 || values[0] != "receipt-v1" {
		return
	}
	owner, ok := wsFallbackOwner(c)
	digest, valid := wsFallbackRequestDigest(c, body, true)
	if !ok || !valid {
		return
	}
	if _, exists := c.Get(wsFallbackObservationKey); !exists {
		c.Set(wsFallbackObservationKey, &wsFallbackObservation{digest: digest, owner: owner})
	}
}

func withWSFallbackObservation(ctx context.Context, c *gin.Context) context.Context {
	if c != nil {
		value, _ := c.Get(wsFallbackObservationKey)
		if observation, ok := value.(*wsFallbackObservation); ok && observation != nil {
			return context.WithValue(ctx, wsFallbackObservationContextKey{}, observation)
		}
	}
	return ctx
}

func markWSFallbackSubmitted(ctx context.Context) {
	if observation, _ := ctx.Value(wsFallbackObservationContextKey{}).(*wsFallbackObservation); observation != nil {
		// Set before the write: an error may mean a partial submission.
		observation.block(wsFallbackSubmitted)
	}
}

func markWSFallbackCancelled(ctx context.Context) {
	if ctx == nil {
		return
	}
	if observation, _ := ctx.Value(wsFallbackObservationContextKey{}).(*wsFallbackObservation); observation != nil {
		observation.block(wsFallbackCancelled)
	}
}

func (o *wsFallbackObservation) block(reason uint32) {
	for {
		phase := o.phase.Load()
		if phase == wsFallbackSubmitted || phase == wsFallbackCancelled || phase == wsFallbackIssued {
			return
		}
		if o.phase.CompareAndSwap(phase, reason) {
			return
		}
	}
}

func markWSFallbackAcquireFailed(ctx context.Context) {
	if observation, _ := ctx.Value(wsFallbackObservationContextKey{}).(*wsFallbackObservation); observation != nil {
		observation.acquireFailed.Store(true)
	}
}

// Called only at a final proxy failure, before the handler's close frame.
// Nothing in this receipt authorizes or instructs replay of a request.
func (s *OpenAIGatewayService) WriteWSFallbackReceipt(c *gin.Context, conn *coderws.Conn, cause error) {
	if s == nil || c == nil || c.Request == nil || conn == nil || cause == nil || c.Request.Context().Err() != nil ||
		IsOpenAIWSClientTurnCancelled(cause) || IsOpenAIWSSessionPreemptedError(cause) || isOpenAIWSClientDisconnectError(cause) {
		return
	}
	var closed *OpenAIWSClientCloseError
	if errors.As(cause, &closed) && closed.StatusCode() == coderws.StatusPolicyViolation {
		return
	}
	value, _ := c.Get(wsFallbackObservationKey)
	observation, _ := value.(*wsFallbackObservation)
	store, supported := s.cache.(WSClientFallbackStore)
	owner, validOwner := wsFallbackOwner(c)
	if !supported || observation == nil || !validOwner || owner != observation.owner ||
		!observation.acquireFailed.Load() || !observation.phase.CompareAndSwap(wsFallbackObserving, wsFallbackPublishing) {
		return
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return
	}
	receipt := "wfc1_" + base64.RawURLEncoding.EncodeToString(secret[:])
	hash, _ := wsFallbackReceiptParts(receipt)
	attempt, err := uuid.NewRandom()
	if err != nil {
		return
	}
	attemptID := attempt.String()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 150*time.Millisecond)
	defer cancel()
	stored, err := store.PutWSFallbackReceipt(ctx, owner[1], hash, wsFallbackBinding(receipt, owner, observation.digest), attemptID)
	if err != nil || !stored {
		return
	}
	// Linearization point: a cancel/submission processed while Redis was
	// pending wins this CAS and prevents issuance. An unexposed stored entry
	// simply expires; no request waits on cleanup or gains authority from it.
	if !observation.phase.CompareAndSwap(wsFallbackPublishing, wsFallbackIssued) {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "error", "error": map[string]string{"type": "server_error", "code": "ws_connection_unavailable", "message": "Upstream WebSocket connection could not be established"},
		"sub4api_fallback": map[string]any{"receipt": receipt, "ws_attempt_id": attemptID, "expires_in_seconds": int(WSFallbackReceiptTTL.Seconds())},
	})
	writeCtx, cancelWrite := context.WithTimeout(c.Request.Context(), 500*time.Millisecond)
	defer cancelWrite()
	if err := conn.Write(writeCtx, coderws.MessageText, payload); err == nil {
		logger.FromContext(c.Request.Context()).Info("openai.websocket_client_fallback_receipt_sent", zap.String("ws_attempt_id", attemptID))
	}
}

// Verification failure is deliberately silent and never rejects the HTTP
// request. Remove private observation headers before any upstream handling.
func (s *OpenAIGatewayService) ObserveClientWSFallback(c *gin.Context, body []byte) {
	if c == nil || c.Request == nil {
		return
	}
	values := c.Request.Header.Values(WSFallbackReceiptHeader)
	stripWSFallbackHeaders(c.Request.Header)
	if _, consumed := c.Get(wsFallbackConsumedKey); consumed {
		return
	}
	c.Set(wsFallbackConsumedKey, true)
	if s == nil || len(values) != 1 || c.Request.Method != http.MethodPost || !strings.HasSuffix(c.Request.URL.Path, "/responses") || GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP {
		return
	}
	receipt := values[0]
	hash, validReceipt := wsFallbackReceiptParts(receipt)
	owner, validOwner := wsFallbackOwner(c)
	store, supported := s.cache.(WSClientFallbackStore)
	if !validReceipt || !validOwner || !supported {
		return
	}
	digest, validPayload := wsFallbackRequestDigest(c, body, false)
	if !validPayload {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 150*time.Millisecond)
	defer cancel()
	attemptID, err := store.ConsumeWSFallbackReceipt(ctx, owner[1], hash, wsFallbackBinding(receipt, owner, digest))
	if err != nil {
		return
	}
	if verified := normalizeUsageClientWSFailure(&UsageClientWSFailure{AttemptID: attemptID, Evidence: "receipt_v1"}); verified != nil {
		c.Set(wsFallbackVerifiedKey, verified)
	}
}

func stripWSFallbackHeaders(headers http.Header) {
	headers.Del(WSFallbackOptInHeader)
	headers.Del(WSFallbackReceiptHeader)
}
