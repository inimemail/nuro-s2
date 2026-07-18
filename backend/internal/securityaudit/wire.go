package securityaudit

import (
	"database/sql"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
)

func ProvideService(
	settingRepo service.SettingRepository,
	settingService *service.SettingService,
	db *sql.DB,
	redisClient *redis.Client,
	encryptor service.SecretEncryptor,
) *Service {
	svc := NewService(settingRepo, db, redisClient, encryptor)
	if settingService != nil {
		settingService.SetOnPromptAuditEnabledUpdate(svc.SetFeatureEnabled)
	}
	return svc
}

var ProviderSet = wire.NewSet(
	ProvideService,
	NewPromptAdminHandler,
)
