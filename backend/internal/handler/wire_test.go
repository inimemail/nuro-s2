package handler

import (
	"reflect"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProvideAdminHandlersWiresUserStepUpDependencies(t *testing.T) {
	userHandler := admin.NewUserHandler(nil, nil, nil, nil)
	totpService := &service.TotpService{}
	userService := &service.UserService{}

	handlers := ProvideAdminHandlers(
		nil,                     // dashboardHandler
		userHandler,             // userHandler
		nil,                     // groupHandler
		&admin.AccountHandler{}, // accountHandler
		nil,                     // announcementHandler
		nil,                     // dataManagementHandler
		nil,                     // backupHandler
		nil,                     // oauthHandler
		nil,                     // openaiOAuthHandler
		nil,                     // geminiOAuthHandler
		nil,                     // antigravityOAuthHandler
		nil,                     // grokOAuthHandler
		nil,                     // proxyHandler
		nil,                     // redeemHandler
		nil,                     // promoHandler
		nil,                     // settingHandler
		nil,                     // opsHandler
		nil,                     // systemHandler
		nil,                     // subscriptionHandler
		nil,                     // usageHandler
		nil,                     // userAttributeHandler
		nil,                     // errorPassthroughHandler
		nil,                     // tlsFingerprintProfileHandler
		nil,                     // apiKeyHandler
		nil,                     // scheduledTestHandler
		nil,                     // channelHandler
		nil,                     // channelMonitorHandler
		nil,                     // channelMonitorTemplateHandler
		nil,                     // contentModerationHandler
		nil,                     // paymentHandler
		nil,                     // affiliateHandler
		nil,                     // auditLogHandler
		nil,                     // promptAuditHandler
		totpService,
		userService,
		nil, // upstreamBillingProbe
	)

	require.Same(t, userHandler, handlers.User)
	value := reflect.ValueOf(userHandler).Elem()
	require.Equal(t, reflect.ValueOf(totpService).Pointer(), value.FieldByName("totpService").Pointer())
	require.Equal(t, reflect.ValueOf(userService).Pointer(), value.FieldByName("userService").Pointer())
}
