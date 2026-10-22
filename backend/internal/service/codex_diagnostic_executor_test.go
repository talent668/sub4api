package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexDiagnosticOutputTerminalVariants(t *testing.T) {
	for _, tc := range []struct {
		name, raw, text string
		complete        bool
	}{
		{"done", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"1, 2\"}\n\ndata: {\"type\":\"response.done\",\"response\":{\"status\":\"completed\"}}\n\n", "1, 2", true},
		{"event name", "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"3, 4\"}]}]}}\n\n", "3, 4", true},
		{"failed done", "data: {\"type\":\"response.done\",\"response\":{\"status\":\"failed\"}}\n\n", "", false},
		{"multiple JSON parts", `{"status":"completed","output":[{"content":[{"type":"output_text","text":"1, "},{"type":"output_text","text":"2"}]},{"content":[{"type":"output_text","text":", 3"}]}]}`, "1, 2, 3", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, complete := CodexDiagnosticOutput([]byte(tc.raw))
			require.Equal(t, tc.text, text)
			require.Equal(t, tc.complete, complete)
		})
	}
}

func TestTruncateScheduledDiagnosticReplyPreservesUTF8(t *testing.T) {
	text := strings.Repeat("测", 501)
	require.Equal(t, strings.Repeat("测", 500), truncateScheduledDiagnosticReply(text))
}

type codexDiagnosticExecutorAccountRepo struct {
	AccountRepository
	deadlineObserved bool
}

func (r *codexDiagnosticExecutorAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	deadline, ok := ctx.Deadline()
	r.deadlineObserved = ok && time.Until(deadline) <= codexDiagnosticRequestTimeout
	return &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Credentials: map[string]any{"chatgpt_account_id": "diagnostic-account"}}, nil
}

func TestCodexDiagnosticProbeBoundsPreparationAndResponse(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "oversized"}[oversized], func(t *testing.T) {
			repo := &codexDiagnosticExecutorAccountRepo{}
			executor := &CodexDiagnosticExecutor{gateway: &OpenAIGatewayService{accountRepo: repo}}
			result := &CodexDiagnosticModelResult{Status: ScheduledDiagnosticStatusFailed}
			ctx := WithCodexDiagnosticRequestSource(context.Background(), "203.0.113.8:4567", "admin.example.test")
			executor.probeModel(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "203.0.113.8:4567", r.RemoteAddr)
				require.Equal(t, "admin.example.test", r.Host)
				if oversized {
					_, err := w.Write([]byte(strings.Repeat("x", codexDiagnosticBodyLimit+1)))
					require.ErrorIs(t, err, errCodexDiagnosticResponseTooLarge)
					require.ErrorIs(t, r.Context().Err(), context.Canceled)
					return
				}
				_, err := w.Write([]byte(`{"status":"completed","output_text":"1, 2"}`))
				require.NoError(t, err)
			}), &APIKey{Key: "test-key"}, 41, "gpt-5.4", func() (ModelTraceChallenge, error) {
				return ModelTraceChallenge{Prompt: "numbers", ExpectedCount: 100}, nil
			}, result)
			require.True(t, repo.deadlineObserved, "account preparation must use the per-model deadline")
			require.False(t, result.StartedAt.IsZero())
			require.False(t, result.FinishedAt.Before(result.StartedAt))
			if oversized {
				require.Equal(t, "response_too_large", result.Reason)
			} else {
				require.Equal(t, "insufficient_numbers", result.Reason)
			}
		})
	}
}

func TestCodexDiagnosticResponseWriterBoundsWrites(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := newCodexDiagnosticResponseWriter(cancel)
	writer.WriteHeader(http.StatusOK)
	_, err := writer.Write([]byte(strings.Repeat("a", codexDiagnosticBodyLimit+1)))
	require.ErrorIs(t, err, errCodexDiagnosticResponseTooLarge)
	require.True(t, writer.overflow)
	require.LessOrEqual(t, writer.body.Len(), codexDiagnosticBodyLimit)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	_, err = writer.Write([]byte("more"))
	require.True(t, errors.Is(err, errCodexDiagnosticResponseTooLarge))
}
