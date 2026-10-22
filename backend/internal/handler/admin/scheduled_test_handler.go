package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

// ScheduledTestHandler handles admin scheduled-test-plan management.
type ScheduledTestHandler struct {
	scheduledTestSvc *service.ScheduledTestService
	apiKeySvc        *service.APIKeyService
}

// NewScheduledTestHandler creates a new ScheduledTestHandler.
func NewScheduledTestHandler(scheduledTestSvc *service.ScheduledTestService, apiKeySvc *service.APIKeyService) *ScheduledTestHandler {
	return &ScheduledTestHandler{scheduledTestSvc: scheduledTestSvc, apiKeySvc: apiKeySvc}
}

type createScheduledTestPlanRequest struct {
	AccountID      int64    `json:"account_id" binding:"required"`
	TestType       string   `json:"test_type"`
	ModelID        string   `json:"model_id"`
	Models         []string `json:"models"`
	APIKeyID       *int64   `json:"api_key_id"`
	CronExpression string   `json:"cron_expression" binding:"required"`
	Enabled        *bool    `json:"enabled"`
	MaxResults     int      `json:"max_results"`
	AutoRecover    *bool    `json:"auto_recover"`
}

type updateScheduledTestPlanRequest struct {
	TestType       string   `json:"test_type"`
	ModelID        string   `json:"model_id"`
	Models         []string `json:"models"`
	APIKeyID       *int64   `json:"api_key_id"`
	CronExpression string   `json:"cron_expression"`
	Enabled        *bool    `json:"enabled"`
	MaxResults     int      `json:"max_results"`
	AutoRecover    *bool    `json:"auto_recover"`
}

func validateScheduledDiagnosticModels(models []string) error {
	if len(models) == 0 {
		return nil
	}
	allowed, err := service.ModelTraceGPTModels()
	if err != nil {
		return err
	}
	modelSet := make(map[string]bool, len(allowed))
	for _, model := range allowed {
		modelSet[model] = true
	}
	for _, model := range models {
		if !modelSet[model] {
			return fmt.Errorf("unsupported GPT model %q", model)
		}
	}
	return nil
}

func (h *ScheduledTestHandler) authorizeDiagnosticPlan(c *gin.Context, plan *service.ScheduledTestPlan) bool {
	if plan.APIKeyID == nil || *plan.APIKeyID <= 0 {
		response.BadRequest(c, "api_key_id is required for codex diagnostic plans")
		return false
	}
	return authorizeCodexDiagnosticAPIKey(c, h.apiKeySvc, *plan.APIKeyID)
}

// ListByAccount GET /admin/accounts/:id/scheduled-test-plans
func (h *ScheduledTestHandler) ListByAccount(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid account id")
		return
	}

	plans, err := h.scheduledTestSvc.ListPlansByAccount(c.Request.Context(), accountID)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, plans)
}

// Create POST /admin/scheduled-test-plans
func (h *ScheduledTestHandler) Create(c *gin.Context) {
	var req createScheduledTestPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	plan := &service.ScheduledTestPlan{
		AccountID:      req.AccountID,
		TestType:       strings.ToLower(strings.TrimSpace(req.TestType)),
		ModelID:        req.ModelID,
		Models:         req.Models,
		APIKeyID:       req.APIKeyID,
		CronExpression: req.CronExpression,
		Enabled:        true,
		MaxResults:     req.MaxResults,
	}
	if req.Enabled != nil {
		plan.Enabled = *req.Enabled
	}
	if req.AutoRecover != nil {
		plan.AutoRecover = *req.AutoRecover
	}
	if err := validateScheduledDiagnosticModels(req.Models); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if plan.TestType == service.ScheduledTestTypeCodexDiagnostic && !h.authorizeDiagnosticPlan(c, plan) {
		return
	}

	created, err := h.scheduledTestSvc.CreatePlan(c.Request.Context(), plan)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, created)
}

// Update PUT /admin/scheduled-test-plans/:id
func (h *ScheduledTestHandler) Update(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	existing, err := h.scheduledTestSvc.GetPlan(c.Request.Context(), planID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}

	var req updateScheduledTestPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// Work on a copy so rejected changes cannot mutate the loaded plan.
	plan := *existing
	existing = &plan
	previousType := strings.ToLower(strings.TrimSpace(existing.TestType))
	previousKeyID := existing.APIKeyID

	if req.TestType != "" {
		existing.TestType = strings.ToLower(strings.TrimSpace(req.TestType))
	}
	if req.ModelID != "" {
		existing.ModelID = req.ModelID
	}
	if req.Models != nil {
		if err := validateScheduledDiagnosticModels(req.Models); err != nil {
			response.BadRequest(c, err.Error())
			return
		}
		existing.Models = req.Models
	}
	if req.APIKeyID != nil {
		existing.APIKeyID = req.APIKeyID
	}
	if req.CronExpression != "" {
		existing.CronExpression = req.CronExpression
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.MaxResults > 0 {
		existing.MaxResults = req.MaxResults
	}
	if req.AutoRecover != nil {
		existing.AutoRecover = *req.AutoRecover
	}

	keyChanged := req.APIKeyID != nil && (previousKeyID == nil || *req.APIKeyID != *previousKeyID)
	// Every enabled diagnostic update must authorize its billing key, including
	// edits to older plans. Disabled plans may keep an unavailable original key.
	if strings.ToLower(strings.TrimSpace(existing.TestType)) == service.ScheduledTestTypeCodexDiagnostic &&
		(existing.Enabled || previousType != service.ScheduledTestTypeCodexDiagnostic || keyChanged) &&
		!h.authorizeDiagnosticPlan(c, existing) {
		return
	}

	updated, err := h.scheduledTestSvc.UpdatePlan(c.Request.Context(), existing)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete DELETE /admin/scheduled-test-plans/:id
func (h *ScheduledTestHandler) Delete(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	if err := h.scheduledTestSvc.DeletePlan(c.Request.Context(), planID); err != nil {
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// ListResults GET /admin/scheduled-test-plans/:id/results
func (h *ScheduledTestHandler) ListResults(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	limit := 50
	if l, err := strconv.Atoi(c.Query("limit")); err == nil && l > 0 {
		limit = l
	}

	results, err := h.scheduledTestSvc.ListResults(c.Request.Context(), planID, limit)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, results)
}
