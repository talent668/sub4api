package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestScanCCStreamPreservesEscapedUsageAndCacheTokens(t *testing.T) {
	for _, key := range []string{`usage`, `us\u0061ge`, `\u0075sage`} {
		t.Run(key, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			body := `data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n" +
				`data: {"choices":[],"` + key + `":{"prompt_tokens":100,"completion_tokens":25,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\ndata: [DONE]\n\n"
			response := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
			defer func() { _ = response.Body.Close() }()
			var svc OpenAIGatewayService
			state := svc.scanCCStream(c, response, "usage regression", "synthetic", time.Now(), func(*apicompat.ChatCompletionsChunk) {})
			require.NoError(t, state.Err)
			require.True(t, state.SawDone)
			require.Equal(t, 100, state.Usage.InputTokens)
			require.Equal(t, 25, state.Usage.OutputTokens)
			require.Equal(t, 80, state.Usage.CacheReadInputTokens)
			require.Nil(t, state.FirstTokenMs, "usage-only chunks are not visible output")
		})
	}
}
