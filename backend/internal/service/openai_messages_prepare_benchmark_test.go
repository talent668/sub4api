package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/apicompat"
)

var (
	openAIMessagesPrepareBenchmarkMap  map[string]any
	openAIMessagesPrepareBenchmarkBody []byte
)

// BenchmarkOpenAIMessagesPrepare measures the whole local preparation segment,
// not just the removed decode: initial marshal, transform decode and mutations,
// remarshal, final policies, and WS map selection (including bytes.Equal). It
// intentionally excludes network, credentials, WS pooling and response handling.
func BenchmarkOpenAIMessagesPrepare(b *testing.B) {
	account := fastWSBridgeTestAccount(true, false)
	svc := &OpenAIGatewayService{}
	for _, fixture := range []struct {
		name     string
		messages int
		textSize int
		tools    int
	}{
		{name: "short", messages: 1, textSize: 64},
		{name: "long_history", messages: 32, textSize: 4096},
		{name: "schema_heavy", messages: 4, textSize: 256, tools: 32},
	} {
		req := openAIMessagesPrepareBenchmarkRequest(b, fixture.messages, fixture.textSize, fixture.tools)
		initial, err := json.Marshal(req)
		if err != nil {
			b.Fatal(err)
		}
		for _, policy := range []struct {
			name      string
			maxEffort string
			tier      string
		}{
			// Forcing the already-present tier exercises comparison against a
			// policy-produced byte slice, not just a same-slice equality shortcut.
			{name: "unchanged", maxEffort: "high", tier: "priority"},
			{name: "changed", maxEffort: "low", tier: "default"},
		} {
			ctx := WithOpenAIReasoningEffortPolicy(fastWSBridgeTestContext(policy.tier), policy.maxEffort, nil, "")
			for _, mode := range []string{"original_redecode", "number_redecode", "snapshot_reuse"} {
				b.Run(fixture.name+"/"+policy.name+"/"+mode, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(initial)))
					for i := 0; i < b.N; i++ {
						body, err := json.Marshal(req)
						if err != nil {
							b.Fatal(err)
						}
						var retained map[string]any
						if mode == "original_redecode" {
							err = json.Unmarshal(body, &retained)
						} else {
							err = decodeOpenAIJSONUseNumber(body, &retained)
						}
						if err != nil {
							b.Fatal(err)
						}
						result := applyCodexOAuthTransformWithOptions(retained, codexOAuthTransformOptions{SkipDefaultInstructions: true, PreserveToolCallIDs: true})
						if result.Error != nil {
							b.Fatal(result.Error)
						}
						if _, err := applyForcedCodexInstructionsTemplate(retained, "", forcedCodexInstructionsTemplateData{}); err != nil {
							b.Fatal(err)
						}
						ensureCodexOAuthInstructionsField(retained)
						appendOpenAICompatClaudeCodeTodoGuardToRequestBody(retained)
						applyCodexAccountIdentityClientMetadataMap(retained, account, 1)
						delete(retained, "prompt_cache_key")
						snapshot, err := json.Marshal(retained)
						if err != nil {
							b.Fatal(err)
						}
						body, _, err = ApplyOpenAIReasoningEffortPolicyFromContext(ctx, snapshot)
						if err != nil {
							b.Fatal(err)
						}
						body, err = svc.applyOpenAIFastPolicyToBody(ctx, account, req.Model, body)
						if err != nil {
							b.Fatal(err)
						}
						var selected map[string]any
						if mode == "snapshot_reuse" {
							selected, err = selectOpenAIMessagesWSRequestBody(body, retained, snapshot)
						} else {
							err = decodeOpenAIJSONUseNumber(body, &selected)
						}
						if err != nil {
							b.Fatal(err)
						}
						openAIMessagesPrepareBenchmarkMap = selected
						openAIMessagesPrepareBenchmarkBody = body
					}
				})
			}
		}
	}
}

func openAIMessagesPrepareBenchmarkRequest(b *testing.B, messages, textSize, toolCount int) *apicompat.ResponsesRequest {
	b.Helper()
	items := make([]apicompat.ResponsesInputItem, messages)
	for i := range items {
		content, err := json.Marshal([]apicompat.ResponsesContentPart{{Type: "input_text", Text: strings.Repeat("x", textSize)}})
		if err != nil {
			b.Fatal(err)
		}
		items[i] = apicompat.ResponsesInputItem{Type: "message", Role: "user", Content: content}
	}
	input, err := json.Marshal(items)
	if err != nil {
		b.Fatal(err)
	}
	tools := make([]apicompat.ResponsesTool, toolCount)
	for i := range tools {
		// Repeated nested schemas exercise map/slice allocation, large integer
		// fidelity and fractional/exponent values, rather than just text bytes.
		properties := make(map[string]json.RawMessage, 16)
		for j := 0; j < 16; j++ {
			properties[fmt.Sprintf("field_%d", j)] = json.RawMessage(openAIMessagesPrepareNumericSchema)
		}
		schema, err := json.Marshal(struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
		}{Type: "object", Properties: properties})
		if err != nil {
			b.Fatal(err)
		}
		tools[i] = apicompat.ResponsesTool{Type: "function", Name: fmt.Sprintf("inspect_%d", i), Parameters: schema}
	}
	return &apicompat.ResponsesRequest{
		Model:        "gpt-6.1-sol",
		Instructions: "Assist with the coding task.",
		Input:        input,
		Stream:       true,
		Tools:        tools,
		Reasoning:    &apicompat.ResponsesReasoning{Effort: "high", Summary: "auto"},
		ServiceTier:  "priority",
	}
}
