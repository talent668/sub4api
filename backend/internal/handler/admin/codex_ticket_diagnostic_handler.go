package admin

import (
	"net/http"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) SetCodexTicketDiagnosticRouter(router http.Handler, keys *service.APIKeyService) {
	h.codexTicketRouter = router
	h.codexTicketAPIKeys = keys
	if h.codexTicketGateway != nil && keys != nil {
		executor := service.NewCodexDiagnosticExecutor(h.codexTicketGateway, keys)
		executor.SetRouter(router)
		h.codexTicketDiagnostic = executor
	}
}

type codexDiagnosticItem struct {
	Model            string                      `json:"model"`
	Status           string                      `json:"status"`
	Reason           string                      `json:"reason,omitempty"`
	PredictedModel   string                      `json:"predicted_model,omitempty"`
	Probability      float64                     `json:"probability,omitempty"`
	ParsedCount      int                         `json:"parsed_number_count,omitempty"`
	HTTPStatus       int                         `json:"http_status,omitempty"`
	GatewayErrorCode string                      `json:"gateway_error_code,omitempty"`
	Harvest          *service.CodexTicketAttempt `json:"harvest,omitempty"`
}

func codexDiagnosticGatewayError(raw []byte) string {
	return service.CodexDiagnosticGatewayError(raw)
}

func codexDiagnosticOutput(raw []byte) (string, bool) {
	return service.CodexDiagnosticOutput(raw)
}

// authorizeCodexDiagnosticAPIKey applies the same billing-key boundary to
// interactive diagnostics and scheduled diagnostic plan changes.
func authorizeCodexDiagnosticAPIKey(c *gin.Context, keys *service.APIKeyService, keyID int64) bool {
	subject, authorized := middleware.GetAuthSubjectFromContext(c)
	if !authorized || subject.UserID <= 0 {
		response.ErrorWithDetails(c, http.StatusForbidden, "Admin user required", "FORBIDDEN", nil)
		return false
	}
	if keys != nil {
		key, err := keys.GetByID(c.Request.Context(), keyID)
		if err == nil && key != nil && key.UserID == subject.UserID && key.IsActive() && !key.IsExpired() && !key.IsQuotaExhausted() && key.Key != "" {
			return true
		}
	}
	response.ErrorWithDetails(c, http.StatusForbidden, "API key unavailable or not owned by this admin", "API_KEY_UNAVAILABLE", nil)
	return false
}

func (h *AccountHandler) DiagnoseCodexModels(c *gin.Context) {
	h.diagnoseCodexModels(c, service.NewModelTraceChallenge)
}

func (h *AccountHandler) diagnoseCodexModels(c *gin.Context, generateChallenge func() (service.ModelTraceChallenge, error)) {
	accountID, ok := codexTicketAccountID(c)
	if !ok {
		return
	}
	if h.codexTicketRouter == nil || h.codexTicketAPIKeys == nil || h.codexTicketGateway == nil {
		codexTicketError(c, service.ErrCodexTicketUnavailable)
		return
	}
	var input struct {
		APIKeyID int64    `json:"api_key_id"`
		Models   []string `json:"models"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || input.APIKeyID <= 0 || len(input.Models) == 0 || len(input.Models) > 32 {
		response.BadRequest(c, "Select an API key and up to 32 models")
		return
	}
	if !authorizeCodexDiagnosticAPIKey(c, h.codexTicketAPIKeys, input.APIKeyID) {
		return
	}
	allowed, err := service.ModelTraceGPTModels()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	modelSet := make(map[string]bool, len(allowed))
	for _, model := range allowed {
		modelSet[model] = true
	}
	seen := make(map[string]bool, len(input.Models))
	for _, model := range input.Models {
		if !modelSet[model] || seen[model] {
			response.BadRequest(c, "Invalid or duplicate GPT model")
			return
		}
		seen[model] = true
	}
	executor := h.codexTicketExecutor()
	if executor == nil {
		response.ErrorWithDetails(c, http.StatusServiceUnavailable, "Codex diagnostic unavailable", "CODEX_DIAGNOSTIC_UNAVAILABLE", nil)
		return
	}
	runResults, runErr := executor.Run(service.WithCodexDiagnosticRequestSource(c.Request.Context(), c.Request.RemoteAddr, c.Request.Host), accountID, input.APIKeyID, input.Models, generateChallenge)
	if runErr != nil {
		response.ErrorWithDetails(c, http.StatusBadRequest, runErr.Error(), "CODEX_DIAGNOSTIC_FAILED", nil)
		return
	}
	results := make([]codexDiagnosticItem, 0, len(runResults))
	for _, r := range runResults {
		item := codexDiagnosticItem{
			Model:            r.Model,
			Status:           r.Status,
			Reason:           r.Reason,
			PredictedModel:   r.PredictedModel,
			ParsedCount:      r.ParsedCount,
			HTTPStatus:       r.HTTPStatus,
			GatewayErrorCode: r.GatewayErrorCode,
		}
		if r.Probability != nil {
			item.Probability = *r.Probability
		}
		results = append(results, item)
	}
	response.Success(c, gin.H{"items": results, "canceled": c.Request.Context().Err() != nil})
}

// codexTicketExecutor lazily builds the shared diagnostic executor. Tests build
// AccountHandler structs directly, so the executor is created on first use when
// the router/keys/gateway were assigned as fields.
func (h *AccountHandler) codexTicketExecutor() *service.CodexDiagnosticExecutor {
	if h.codexTicketDiagnostic != nil {
		return h.codexTicketDiagnostic
	}
	if h.codexTicketGateway == nil || h.codexTicketAPIKeys == nil || h.codexTicketRouter == nil {
		return nil
	}
	executor := service.NewCodexDiagnosticExecutor(h.codexTicketGateway, h.codexTicketAPIKeys)
	executor.SetRouter(h.codexTicketRouter)
	h.codexTicketDiagnostic = executor
	return executor
}
