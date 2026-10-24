package service

import (
	"context"

	"github.com/gin-gonic/gin"
)

const OpenAINativeWSPreferredExtraKey = "openai_native_ws_preferred"
const openAINativeWSPreferredContextKey = "openai_native_ws_preferred"

type nativeWSPreferredRequestContextKey struct{}

func (a *Account) IsOpenAINativeWSPreferred() bool {
	if a == nil || a.Platform != PlatformOpenAI {
		return false
	}
	value, _ := a.Extra[OpenAINativeWSPreferredExtraKey].(bool)
	return value
}

func latchOpenAINativeWSPreferred(c *gin.Context, a *Account) {
	if c != nil && a.IsOpenAINativeWSPreferred() {
		c.Set(openAINativeWSPreferredContextKey, true)
	}
}

// A fallback is a new HTTP request made by the client. Keep HTTP ingress on
// HTTP even when the global bridge or a group-forced Fast tier is enabled.
// Latch only for this request's account attempts; no account or global settings
// are changed, and no connection state or credentials cross request boundaries.
func keepOpenAIHTTPIngressOnHTTP(ctx context.Context, c *gin.Context, a *Account) bool {
	if a.IsOpenAINativeWSPreferred() {
		return true
	}
	if c != nil {
		if value, exists := c.Get(openAINativeWSPreferredContextKey); exists && value == true {
			return true
		}
	}
	if ctx != nil {
		value, _ := ctx.Value(nativeWSPreferredRequestContextKey{}).(bool)
		return value
	}
	return false
}
