package admin

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

// ScheduledTestHandler handles admin scheduled-test-plan management.
type ScheduledTestHandler struct {
	scheduledTestSvc *service.ScheduledTestService
}

// NewScheduledTestHandler creates a new ScheduledTestHandler.
func NewScheduledTestHandler(scheduledTestSvc *service.ScheduledTestService) *ScheduledTestHandler {
	return &ScheduledTestHandler{scheduledTestSvc: scheduledTestSvc}
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
		TestType:       req.TestType,
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

	if req.TestType != "" {
		existing.TestType = req.TestType
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
