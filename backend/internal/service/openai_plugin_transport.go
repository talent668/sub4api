package service

import (
	"errors"
	"net/http"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	stripWSFallbackHeaders(request.Header)
	required, _ := request.Context().Value(nativeWSRequiredRequestContextKey{}).(bool)
	if account.IsOpenAINativeWSOnly() || required {
		return nil, errors.New("native WS-only account cannot use an HTTP upstream")
	}
	observeUsageTransport(request.Context(), "http", "")
	if err := applyKimiOAuthRequestHeaders(request, account); err != nil {
		return nil, err
	}
	markWSFallbackSubmitted(request.Context())
	snapshot := snapshotCodexTicketHTTPRequest(request, account)
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			s.observeCodexTicketHTTPResponse(snapshot, response)
			return response, err
		}
	}
	response, err := s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	s.observeCodexTicketHTTPResponse(snapshot, response)
	return response, err
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if account.IsKimiOAuth() {
		if s.openaiGatewayService == nil {
			return nil, errors.New("kimi OAuth is unavailable")
		}
		fresh, err := s.openaiGatewayService.kimiOAuthService.GetAccount(request.Context(), account, false)
		if err != nil {
			return nil, err
		}
		account = fresh
		proxyURL = ""
		if account.ProxyID != nil {
			proxyURL = account.Proxy.URL()
		}
	}
	if err := applyKimiOAuthRequestHeaders(request, account); err != nil {
		return nil, err
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
