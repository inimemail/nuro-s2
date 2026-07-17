package securityaudit

import (
	"database/sql"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
)

func ProvideService(
	settingRepo service.SettingRepository,
	db *sql.DB,
	redisClient *redis.Client,
	encryptor service.SecretEncryptor,
) *Service {
	return NewService(settingRepo, db, redisClient, encryptor)
}

var ProviderSet = wire.NewSet(
	ProvideService,
	NewPromptAdminHandler,
)
