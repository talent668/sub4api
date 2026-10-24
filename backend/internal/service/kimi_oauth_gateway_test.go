//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKimiOAuthGatewayUsesOfficialEndpointForEachProtocol(t *testing.T) {
	for _, protocol := range []string{APIProtocolChatCompletions, APIProtocolResponses, APIProtocolAnthropic} {
		t.Run(protocol, func(t *testing.T) {
			a := kimiTestAccount()
			a.Credentials["expires_at"] = time.Now().Add(time.Hour).Unix()
			a.GroupIDs = []int64{7}
			a.Concurrency = 1
			oauth, _, _, locks := newKimiTestFlow()
			repo := &kimiTestAccountRepo{account: a}
			oauth.accounts = repo
			oauth.refreshAPI = NewOAuthRefreshAPI(repo, locks)
			var path, body, response string
			switch protocol {
			case APIProtocolChatCompletions:
				path = "/v1/chat/completions"
				body = `{"model":"kimi-for-coding","messages":[{"role":"user","content":"hello"}],"stream":false}`
				response = `{"id":"c1","object":"chat.completion","model":"kimi-for-coding","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`
			case APIProtocolResponses:
				path = "/v1/responses"
				body = `{"model":"kimi-for-coding","input":"hello","stream":false}`
				response = `{"id":"r1","object":"response","status":"completed","model":"kimi-for-coding","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":2,"output_tokens":1}}`
			default:
				path = "/v1/messages"
				body = `{"model":"kimi-for-coding","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`
				response = `{"id":"m1","type":"message","role":"assistant","model":"kimi-for-coding","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), accountRepo: repo, httpUpstream: upstream, kimiOAuthService: oauth}
			c := adaptiveProtocolTestContext(path, []byte(body))
			groupID := int64(7)
			c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})
			var result *OpenAIForwardResult
			var err error
			switch protocol {
			case APIProtocolChatCompletions:
				result, err = svc.ForwardAsChatCompletions(context.Background(), c, a, []byte(body), "", "")
			case APIProtocolResponses:
				result, err = svc.Forward(context.Background(), c, a, []byte(body))
			default:
				result, err = svc.ForwardAsAnthropic(context.Background(), c, a, []byte(body), "", "")
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, "api.kimi.com", upstream.lastReq.URL.Host)
			require.Equal(t, "https", upstream.lastReq.URL.Scheme)
			require.Equal(t, "Bearer old-access", upstream.lastReq.Header.Get("Authorization"))
			require.Empty(t, upstream.lastReq.Header.Get("X-Api-Key"))
			require.Equal(t, a.GetCredential("device_id"), upstream.lastReq.Header.Get("X-Msh-Device-Id"))
			require.True(t, HTTPUpstreamRedirectsDisabled(upstream.lastReq.Context()))
			require.Equal(t, 1, result.Usage.OutputTokens)
		})
	}
}

func TestKimiOAuthWebSocketResolverKeepsHTTPTransport(t *testing.T) {
	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	decision := NewOpenAIWSProtocolResolver(cfg).Resolve(kimiTestAccount())
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, decision.Transport)
}

func TestKimiOAuthGatewayRejectsGroupChangeBeforeSendingCredential(t *testing.T) {
	a := kimiTestAccount()
	a.Credentials["expires_at"] = time.Now().Add(time.Hour).Unix()
	a.GroupIDs = []int64{99}
	oauth, _, _, locks := newKimiTestFlow()
	repo := &kimiTestAccountRepo{account: a}
	oauth.accounts = repo
	oauth.refreshAPI = NewOAuthRefreshAPI(repo, locks)
	svc := &OpenAIGatewayService{accountRepo: repo, kimiOAuthService: oauth}
	c := adaptiveProtocolTestContext("/v1/responses", nil)
	groupID := int64(7)
	c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})
	_, err := svc.prepareKimiOAuthAccount(context.Background(), c, a)
	require.ErrorContains(t, err, "group changed")
}

func TestKimiOAuthAdminProbeUsesRefreshedCredentials(t *testing.T) {
	a := kimiTestAccount()
	oauth, _, _, locks := newKimiTestFlow()
	repo := &kimiTestAccountRepo{account: a}
	oauth.accounts = repo
	oauth.refreshAPI = NewOAuthRefreshAPI(repo, locks)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[]}`))}}
	service := &AccountTestService{openaiGatewayService: &OpenAIGatewayService{kimiOAuthService: oauth}, httpUpstream: upstream}
	req, _ := http.NewRequest(http.MethodGet, DefaultKimiCodingBaseURL+"/models", nil)
	req.Header.Set("Authorization", "Bearer old-access")
	response, err := service.doOpenAIAccountTestUpstream(req, "", a, false)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, "Bearer new-access", upstream.lastReq.Header.Get("Authorization"))
	require.True(t, HTTPUpstreamRedirectsDisabled(upstream.lastReq.Context()))
}
