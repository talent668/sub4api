package admin

import (
	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type KimiOAuthHandler struct{ oauth *service.KimiOAuthService }

func (h *AccountHandler) SetKimiOAuthService(oauth *service.KimiOAuthService) {
	h.kimiOAuthService = oauth
}
func NewKimiOAuthHandler(oauth *service.KimiOAuthService) *KimiOAuthHandler {
	return &KimiOAuthHandler{oauth: oauth}
}

func (h *KimiOAuthHandler) StartDevice(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "Authentication required")
		return
	}
	var req struct {
		ProxyID *int64 `json:"proxy_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid authorization request")
		return
	}
	result, err := h.oauth.StartDevice(c.Request.Context(), subject.UserID, req.ProxyID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, result)
}

func (h *KimiOAuthHandler) PollDevice(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "Authentication required")
		return
	}
	var req struct {
		SessionID string `json:"session_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid authorization session")
		return
	}
	result, err := h.oauth.PollDevice(c.Request.Context(), subject.UserID, req.SessionID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, result)
}
