package service

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSelectOpenAIMessagesWSRequestBody_ExactSnapshot(t *testing.T) {
	snapshot := []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"high"},"service_tier":"priority"}`)
	for _, tt := range []struct {
		name     string
		body     []byte
		retained bool
		snapshot []byte
		reuse    bool
	}{
		{name: "same bytes", body: snapshot, retained: true, snapshot: snapshot, reuse: true},
		{name: "equal bytes in different allocation", body: bytes.Clone(snapshot), retained: true, snapshot: snapshot, reuse: true},
		{name: "changed effort", body: []byte(`{"model":"gpt-6.1-sol","reasoning":{"effort":"low"},"service_tier":"priority"}`), retained: true, snapshot: snapshot},
		{name: "semantically equal but different bytes", body: append(bytes.Clone(snapshot), '\n'), retained: true, snapshot: snapshot},
		{name: "no retained map", body: snapshot, snapshot: snapshot},
		{name: "no snapshot", body: snapshot, retained: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var retained map[string]any
			if tt.retained {
				require.NoError(t, decodeOpenAIJSONUseNumber(snapshot, &retained))
			}
			selected, err := selectOpenAIMessagesWSRequestBody(tt.body, retained, tt.snapshot)
			require.NoError(t, err)
			var expected map[string]any
			require.NoError(t, decodeOpenAIJSONUseNumber(tt.body, &expected))
			require.Equal(t, expected, selected)
			selected["reuse_probe"] = true
			_, shared := retained["reuse_probe"]
			require.Equal(t, tt.reuse, shared)
		})
	}
}

func TestSelectOpenAIMessagesWSRequestBody_DecodeFallback(t *testing.T) {
	retained := map[string]any{"model": "stale"}
	for _, body := range []string{"", `{`, `[]`, `{"model":"new"} {"extra":true}`, `{"model":"new"} trailing`} {
		t.Run(body, func(t *testing.T) {
			selected, err := selectOpenAIMessagesWSRequestBody([]byte(body), retained, []byte(`{"model":"stale"}`))
			require.Error(t, err)
			require.Nil(t, selected)
			require.Equal(t, "stale", retained["model"])
		})
	}
	selected, err := selectOpenAIMessagesWSRequestBody([]byte(`null`), retained, nil)
	require.NoError(t, err)
	require.Nil(t, selected, "preserve the existing nil-map bridge guard")
}

func TestSelectOpenAIMessagesWSRequestBody_FinalPolicies(t *testing.T) {
	for _, tt := range []struct {
		name       string
		maxEffort  string
		action     string
		forcedTier string
		wantEffort string
		wantTier   string
		wantReuse  bool
	}{
		{name: "unchanged policies", maxEffort: "high", action: BetaPolicyActionPass, wantEffort: "high", wantTier: "priority", wantReuse: true},
		{name: "force identical tier", maxEffort: "high", action: BetaPolicyActionPass, forcedTier: "priority", wantEffort: "high", wantTier: "priority", wantReuse: true},
		{name: "reasoning downgrade", maxEffort: "low", action: BetaPolicyActionPass, wantEffort: "low", wantTier: "priority"},
		{name: "fast filter", action: BetaPolicyActionFilter, wantEffort: "high"},
		{name: "forced tier", action: BetaPolicyActionPass, forcedTier: "default", wantEffort: "high", wantTier: "default"},
		{name: "reasoning then forced tier", maxEffort: "low", action: BetaPolicyActionPass, forcedTier: "default", wantEffort: "low", wantTier: "default"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
				ServiceTier: OpenAIFastTierPriority,
				Action:      tt.action,
				Scope:       BetaPolicyScopeAll,
			}}})
			account := fastWSBridgeTestAccount(true, false)
			ctx := WithOpenAIReasoningEffortPolicy(fastWSBridgeTestContext(tt.forcedTier), tt.maxEffort, nil, "")
			snapshot := []byte(`{"client_metadata":{"session_id":"original"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"model":"gpt-6.1-sol","reasoning":{"effort":"high"},"service_tier":"priority"}`)
			snapshotBefore := bytes.Clone(snapshot)
			var retained map[string]any
			require.NoError(t, decodeOpenAIJSONUseNumber(snapshot, &retained))
			finalBody, _, err := ApplyOpenAIReasoningEffortPolicyFromContext(ctx, snapshot)
			require.NoError(t, err)
			finalBody, err = svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6.1-sol", finalBody)
			require.NoError(t, err)
			finalBefore := bytes.Clone(finalBody)
			selected, err := selectOpenAIMessagesWSRequestBody(finalBody, retained, snapshot)
			require.NoError(t, err)
			require.Equal(t, tt.wantEffort, requireMessagesPrepareType[map[string]any](t, selected["reasoning"])["effort"])
			require.Equal(t, tt.wantTier, gjson.GetBytes(finalBody, "service_tier").String())
			if tt.wantTier == "" {
				require.NotContains(t, selected, "service_tier")
			} else {
				require.Equal(t, tt.wantTier, selected["service_tier"])
			}
			selected["reuse_probe"] = true
			_, shared := retained["reuse_probe"]
			require.Equal(t, tt.wantReuse, shared)

			// Exercise the real shallow-copy/nested-mutation WS preparation. An
			// HTTP fallback must still decode solely from the final policy bytes.
			payload := svc.buildOpenAIWSCreatePayload(selected, account)
			setOpenAIWSTurnMetadata(payload, "ws-only")
			selectedInput := requireMessagesPrepareType[[]any](t, selected["input"])
			requireMessagesPrepareType[map[string]any](t, selectedInput[0])["role"] = "developer"
			require.Equal(t, snapshotBefore, snapshot)
			require.Equal(t, finalBefore, finalBody)
			fallback, err := selectOpenAIMessagesWSRequestBody(finalBody, nil, nil)
			require.NoError(t, err)
			require.Equal(t, tt.wantEffort, requireMessagesPrepareType[map[string]any](t, fallback["reasoning"])["effort"])
			fallbackInput := requireMessagesPrepareType[[]any](t, fallback["input"])
			require.Equal(t, "user", requireMessagesPrepareType[map[string]any](t, fallbackInput[0])["role"])
			require.NotContains(t, fallback["client_metadata"], openAIWSTurnMetadataHeader)
		})
	}
}

const openAIMessagesPrepareNumericSchema = `{"type":"object","properties":{"large":{"type":"integer","const":9007199254740993},"fraction":{"type":"number","multipleOf":0.1234567890123456789},"exponent":{"type":"number","maximum":1.234567890123456789e+40},"nested":{"type":"array","items":{"type":"object","properties":{"signed":{"enum":[-9007199254740993,2.5000,6.02214076e+23]}}}}}}`

func requireMessagesPrepareType[T any](t *testing.T, value any) T {
	t.Helper()
	typed, ok := value.(T)
	require.True(t, ok, "unexpected Messages preparation value type: %T", value)
	return typed
}

func requireOpenAIMessagesPrepareNumericSchema(t *testing.T, body []byte) {
	t.Helper()
	for path, want := range map[string]string{
		"tools.0.parameters.properties.large.const":                           "9007199254740993",
		"tools.0.parameters.properties.fraction.multipleOf":                   "0.1234567890123456789",
		"tools.0.parameters.properties.exponent.maximum":                      "1.234567890123456789e+40",
		"tools.0.parameters.properties.nested.items.properties.signed.enum.0": "-9007199254740993",
		"tools.0.parameters.properties.nested.items.properties.signed.enum.1": "2.5000",
		"tools.0.parameters.properties.nested.items.properties.signed.enum.2": "6.02214076e+23",
	} {
		require.Equal(t, want, gjson.GetBytes(body, path).Raw, path)
	}
}

func TestOpenAIMessagesPrepare_TransformNumbers(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":9007199254740993}]}],"tools":[{"type":"function","name":"inspect","parameters":` + openAIMessagesPrepareNumericSchema + `}]}`)
	var retained map[string]any
	require.NoError(t, decodeOpenAIJSONUseNumber(body, &retained))
	result := applyCodexOAuthTransformWithOptions(retained, codexOAuthTransformOptions{SkipDefaultInstructions: true, PreserveToolCallIDs: true})
	require.NoError(t, result.Error)
	ensureCodexOAuthInstructionsField(retained)
	appendOpenAICompatClaudeCodeTodoGuardToRequestBody(retained)
	snapshot, err := json.Marshal(retained)
	require.NoError(t, err)
	requireOpenAIMessagesPrepareNumericSchema(t, snapshot)
	require.Equal(t, "9007199254740993", gjson.GetBytes(snapshot, "input.1.content.0.text").String(), "content stringify helper accepts json.Number")
	for _, changed := range []bool{false, true} {
		finalBody := snapshot
		if changed {
			finalBody, err = (&OpenAIGatewayService{}).applyOpenAIFastPolicyToBody(fastWSBridgeTestContext("priority"), fastWSBridgeTestAccount(true, false), "gpt-6.1-sol", snapshot)
			require.NoError(t, err)
		}
		selected, err := selectOpenAIMessagesWSRequestBody(finalBody, retained, snapshot)
		require.NoError(t, err)
		tools := requireMessagesPrepareType[[]any](t, selected["tools"])
		tool := requireMessagesPrepareType[map[string]any](t, tools[0])
		parameters := requireMessagesPrepareType[map[string]any](t, tool["parameters"])
		properties := requireMessagesPrepareType[map[string]any](t, parameters["properties"])
		number := requireMessagesPrepareType[map[string]any](t, properties["large"])["const"]
		require.IsType(t, json.Number(""), number)
		selectedBody, err := json.Marshal(selected)
		require.NoError(t, err)
		requireOpenAIMessagesPrepareNumericSchema(t, selectedBody)
	}
}

func TestOpenAIMessagesPrepare_ForwardNumbers(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "ws"
		if fallback {
			name = "http fallback"
		}
		t.Run(name, func(t *testing.T) {
			cfg := newOpenAIWSV2TestConfig()
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			svc, upstream, _ := newFastWSBridgeTestService(t, cfg, conn)
			if fallback {
				useAnthropicMessagesBridgeSSEUpstream(upstream)
				useFastWSBridgeTestDialer(svc, cfg, &sequenceFastWSBridgeDialer{})
			}
			body := []byte(`{"model":"gpt-6.1-sol","max_tokens":64,"stream":true,"speed":"fast","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"inspect","input_schema":` + openAIMessagesPrepareNumericSchema + `}]}`)
			c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
			result, err := svc.ForwardAsAnthropic(context.Background(), c, fastWSBridgeTestAccount(true, false), body, "", "gpt-6.1-sol")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, !fallback, result.OpenAIWSMode)
			if fallback {
				requireOpenAIMessagesPrepareNumericSchema(t, upstream.lastBody)
			} else {
				require.Nil(t, upstream.lastReq)
				written, err := json.Marshal(conn.lastWrite)
				require.NoError(t, err)
				requireOpenAIMessagesPrepareNumericSchema(t, written)
			}
		})
	}
}
