package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type upstreamBillingProbeEnabledRequest struct {
	Enabled *bool `json:"enabled" binding:"required"`
}

type upstreamBillingProbeBatchRequest struct {
	AccountIDs []int64 `json:"account_ids" binding:"required"`
}

type upstreamBillingGuardResponse struct {
	AccountID int64                                 `json:"account_id"`
	Account   AccountWithConcurrency                `json:"account"`
	Snapshot  *service.UpstreamBillingProbeSnapshot `json:"snapshot,omitempty"`
}

type upstreamBillingProbeResponse struct {
	AccountID int64                                 `json:"account_id"`
	Account   *AccountWithConcurrency               `json:"account,omitempty"`
	Snapshot  *service.UpstreamBillingProbeSnapshot `json:"snapshot,omitempty"`
}

func (h *AccountHandler) SetUpstreamBillingProbeService(probe *service.UpstreamBillingProbeService) {
	h.upstreamBillingProbe = probe
}

func (h *AccountHandler) GetUpstreamBillingProbeSettings(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	settings, err := h.upstreamBillingProbe.GetSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *AccountHandler) UpdateUpstreamBillingProbeSettings(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	var req service.UpstreamBillingProbeSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.upstreamBillingProbe.UpdateSettings(c.Request.Context(), &req); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, req)
}

func (h *AccountHandler) SetUpstreamBillingProbeEnabled(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req upstreamBillingProbeEnabledRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if err := h.upstreamBillingProbe.SetAccountEnabled(c.Request.Context(), id, *req.Enabled); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"account_id": id, "enabled": *req.Enabled})
}

func (h *AccountHandler) UpdateUpstreamBillingGuard(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req service.UpstreamBillingGuardSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := h.upstreamBillingProbe.UpdateGuard(c.Request.Context(), id, req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, upstreamBillingGuardResponse{
		AccountID: result.AccountID,
		Account:   h.buildAccountResponseWithRuntime(c.Request.Context(), result.Account),
		Snapshot:  result.Snapshot,
	})
}

func (h *AccountHandler) ProbeUpstreamBilling(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	snapshot, err := h.upstreamBillingProbe.ProbeAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	result := upstreamBillingProbeResponse{AccountID: id, Snapshot: snapshot}
	if account, loadErr := h.adminService.GetAccount(c.Request.Context(), id); loadErr == nil && account != nil {
		accountResponse := h.buildAccountResponseWithRuntime(c.Request.Context(), account)
		result.Account = &accountResponse
	}
	response.Success(c, result)
}

func (h *AccountHandler) ProbeUpstreamBillingBatch(c *gin.Context) {
	if h.upstreamBillingProbe == nil {
		response.ErrorFrom(c, service.ErrUpstreamBillingProbeUnavailable)
		return
	}
	var req upstreamBillingProbeBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if len(req.AccountIDs) == 0 || len(req.AccountIDs) > service.UpstreamBillingProbeMaxBatchSize {
		response.BadRequest(c, "account_ids must contain between 1 and 20 items")
		return
	}
	seen := make(map[int64]struct{}, len(req.AccountIDs))
	ids := make([]int64, 0, len(req.AccountIDs))
	for _, id := range req.AccountIDs {
		if id <= 0 {
			response.BadRequest(c, "account_ids must contain positive IDs")
			return
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	response.Success(c, gin.H{"results": h.upstreamBillingProbe.ProbeAccounts(c.Request.Context(), ids)})
}
