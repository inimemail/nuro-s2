package admin

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) Duplicate(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	svc, ok := h.adminService.(service.AdminAccountDuplicateService)
	if !ok {
		response.ErrorFrom(c, fmt.Errorf("account duplicate service is unavailable"))
		return
	}
	actor, key := adminActorScope(c), c.GetHeader("Idempotency-Key")
	result, err := executeAdminIdempotent(c, "admin.accounts.duplicate", struct {
		AccountID int64 `json:"account_id"`
	}{id}, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		account, err := svc.DuplicateAccount(ctx, id, actor, key)
		if err != nil {
			return nil, err
		}
		return h.buildAccountResponseWithRuntime(ctx, account), nil
	})
	if err != nil {
		reason := infraerrors.Reason(err)
		if reason == infraerrors.Reason(service.ErrIdempotencyInProgress) || reason == infraerrors.Reason(service.ErrIdempotencyStoreUnavail) {
			account, recoverErr := svc.RecoverDuplicateAccount(c.Request.Context(), id, actor, key)
			if recoverErr != nil {
				slog.Warn("account_duplicate_recovery_failed", "account_id", id, "error", recoverErr)
			} else if account != nil {
				c.Header("X-Idempotency-Recovered", "true")
				response.Success(c, h.buildAccountResponseWithRuntime(c.Request.Context(), account))
				return
			}
		}
		response.ErrorFrom(c, err)
		return
	}
	if result.Replayed {
		c.Header("X-Idempotency-Replayed", "true")
	}
	response.Success(c, result.Data)
}
