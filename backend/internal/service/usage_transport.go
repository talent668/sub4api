package service

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// UsageTransportDetails contains only observed protocol enums. It never stores
// URLs, headers, payloads, or credentials, and is not used for billing/routing.
type UsageTransportDetails struct {
	Client          string                `json:"client"`
	Upstream        string                `json:"upstream"`
	Mode            string                `json:"mode,omitempty"`
	FallbackReason  string                `json:"fallback_reason,omitempty"`
	ClientWSFailure *UsageClientWSFailure `json:"client_ws_failure,omitempty"`
}

func newUsageTransportDetails(client, upstream, mode, reason string) *UsageTransportDetails {
	return NormalizeUsageTransportDetails(&UsageTransportDetails{Client: client, Upstream: upstream, Mode: mode, FallbackReason: reason})
}

// NormalizeUsageTransportDetails projects both new and historical JSON through
// allowlists. Missing/invalid observations remain unknown instead of guessing.
func NormalizeUsageTransportDetails(d *UsageTransportDetails) *UsageTransportDetails {
	if d == nil || (d.Client != "http" && d.Client != "ws") || (d.Upstream != "http" && d.Upstream != "ws") {
		return nil
	}
	out := &UsageTransportDetails{Client: d.Client, Upstream: d.Upstream}
	if d.Client == "http" {
		out.ClientWSFailure = normalizeUsageClientWSFailure(d.ClientWSFailure)
	}
	if d.Client == "ws" && d.Upstream == "ws" && (d.Mode == "ctx_pool" || d.Mode == "passthrough") {
		out.Mode = d.Mode
	}
	if d.Client == "ws" && d.Upstream == "http" && d.Mode == "http_bridge" {
		out.Mode = d.Mode
	}
	if d.Client == "http" && d.Upstream == "ws" && d.Mode == "http_ws_bridge" {
		out.Mode = d.Mode
	}
	if d.Client == "ws" && d.Upstream == "http" {
		switch d.FallbackReason {
		case "large_payload", "plugin_http_bridge", "provider_http_bridge":
			out.FallbackReason = d.FallbackReason
		}
	}
	if d.Client == "http" && d.Upstream == "http" && d.FallbackReason == "ws_failure" {
		out.FallbackReason = d.FallbackReason
	}
	return out
}

func UsageTransportDetailsJSON(d *UsageTransportDetails) any {
	d = NormalizeUsageTransportDetails(d)
	if d == nil {
		return nil
	}
	value, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	return string(value)
}

func ParseUsageTransportDetailsJSON(value string) *UsageTransportDetails {
	if len(value) > 1024 {
		return nil
	}
	var d UsageTransportDetails
	if json.Unmarshal([]byte(value), &d) != nil {
		return nil
	}
	return NormalizeUsageTransportDetails(&d)
}

type usageTransportTraceKey struct{}
type usageTransportTrace struct {
	client          string
	value           atomic.Pointer[UsageTransportDetails]
	clientWSFailure *UsageClientWSFailure
}

func withUsageTransportTrace(ctx context.Context, c *gin.Context) (context.Context, *usageTransportTrace) {
	if keepOpenAIHTTPIngressOnHTTP(ctx, c, nil) {
		ctx = context.WithValue(ctx, nativeWSPreferredRequestContextKey{}, true)
	}
	if isOpenAINativeWSRequired(c, nil) {
		ctx = context.WithValue(ctx, nativeWSRequiredRequestContextKey{}, true)
	}
	if trace, _ := ctx.Value(usageTransportTraceKey{}).(*usageTransportTrace); trace != nil {
		return ctx, trace
	}
	client := string(GetOpenAIClientTransport(c))
	if client == "" && c != nil && c.Request != nil {
		client = "http"
	}
	trace := &usageTransportTrace{client: client}
	if c != nil && client == "http" {
		verified, _ := c.Get(wsFallbackVerifiedKey)
		if failure, ok := verified.(*UsageClientWSFailure); ok {
			trace.clientWSFailure = normalizeUsageClientWSFailure(failure)
		}
	}
	return context.WithValue(ctx, usageTransportTraceKey{}, trace), trace
}

func observeUsageTransport(ctx context.Context, upstream, mode string) {
	trace, _ := ctx.Value(usageTransportTraceKey{}).(*usageTransportTrace)
	if trace == nil {
		return
	}
	reason := ""
	if previous := trace.value.Load(); previous != nil && previous.Upstream == "ws" && upstream == "http" {
		reason = "ws_failure"
	} else if previous != nil && previous.Upstream == "http" && upstream == "http" {
		reason = previous.FallbackReason
	}
	if trace.client == "http" && upstream == "ws" {
		mode = "http_ws_bridge"
	}
	trace.value.Store(newUsageTransportDetails(trace.client, upstream, mode, reason))
}

func observeUsageTransportFallback(ctx context.Context, reason string) {
	trace, _ := ctx.Value(usageTransportTraceKey{}).(*usageTransportTrace)
	if trace == nil {
		return
	}
	trace.value.Store(newUsageTransportDetails(trace.client, "ws", "http_ws_bridge", reason))
}

func (t *usageTransportTrace) finish(result *OpenAIForwardResult) *OpenAIForwardResult {
	if result != nil && result.TransportDetails == nil {
		result.TransportDetails = NormalizeUsageTransportDetails(t.value.Load())
	}
	if result != nil && result.TransportDetails != nil && result.TransportDetails.Client == "http" && t.clientWSFailure != nil {
		details := NormalizeUsageTransportDetails(result.TransportDetails)
		if details != nil {
			details.ClientWSFailure = normalizeUsageClientWSFailure(t.clientWSFailure)
			result.TransportDetails = details
		}
	}
	return result
}

func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	if err := s.requireNativeWSClient(c, account, "responses"); err != nil {
		return nil, err
	}
	ctx, trace := withUsageTransportTrace(ctx, c)
	result, err := s.forwardWithTransportObservation(ctx, c, account, body)
	return trace.finish(result), err
}

func (s *OpenAIGatewayService) ForwardAsAnthropic(ctx context.Context, c *gin.Context, account *Account, body []byte, key, model string) (*OpenAIForwardResult, error) {
	if err := s.requireNativeWSClient(c, account, "anthropic"); err != nil {
		return nil, err
	}
	ctx, trace := withUsageTransportTrace(ctx, c)
	result, err := s.forwardAsAnthropicWithTransportObservation(ctx, c, account, body, key, model)
	return trace.finish(result), err
}
