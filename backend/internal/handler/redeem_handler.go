package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"strconv"

	"github.com/gin-gonic/gin"
)

// RedeemHandler handles redeem code-related requests
type RedeemHandler struct {
	redeemService *service.RedeemService
}

// NewRedeemHandler creates a new RedeemHandler
func NewRedeemHandler(redeemService *service.RedeemService) *RedeemHandler {
	return &RedeemHandler{
		redeemService: redeemService,
	}
}

// RedeemRequest represents the redeem code request payload
type RedeemRequest struct {
	Code string `json:"code" binding:"required"`
}

// RedeemResponse represents the redeem response
type RedeemResponse struct {
	Message        string   `json:"message"`
	Type           string   `json:"type"`
	Value          float64  `json:"value"`
	NewBalance     *float64 `json:"new_balance,omitempty"`
	NewConcurrency *int     `json:"new_concurrency,omitempty"`
}

// Redeem handles redeeming a code
// POST /api/v1/redeem
func (h *RedeemHandler) Redeem(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	var req RedeemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.redeemService.Redeem(c.Request.Context(), subject.UserID, req.Code)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.RedeemCodeFromService(result))
}

// GetHistory returns the user's redemption history
// GET /api/v1/redeem/history
func (h *RedeemHandler) GetHistory(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	if c.Request.URL.Query().Has("page") || c.Request.URL.Query().Has("page_size") {
		page, e1 := strconv.Atoi(c.DefaultQuery("page", "1"))
		size, e2 := strconv.Atoi(c.DefaultQuery("page_size", "25"))
		if e1 != nil || e2 != nil || page < 1 || size < 1 || size > 100 || page-1 > int(^uint(0)>>1)/size {
			response.BadRequest(c, "Invalid pagination: page must be positive and page_size must be 1–100")
			return
		}
		codes, paging, err := h.redeemService.GetUserHistoryPaginated(c.Request.Context(), subject.UserID, pagination.PaginationParams{Page: page, PageSize: size})
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		out := make([]dto.RedeemCode, 0, len(codes))
		for i := range codes {
			out = append(out, *dto.RedeemCodeFromService(&codes[i]))
		}
		response.Success(c, response.PaginatedData{Items: out, Total: paging.Total, Page: paging.Page, PageSize: paging.PageSize, Pages: paging.Pages})
		return
	}
	// Preserve the legacy array response for callers without pagination.
	limit := 25

	codes, err := h.redeemService.GetUserHistory(c.Request.Context(), subject.UserID, limit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.RedeemCode, 0, len(codes))
	for i := range codes {
		out = append(out, *dto.RedeemCodeFromService(&codes[i]))
	}
	response.Success(c, out)
}
