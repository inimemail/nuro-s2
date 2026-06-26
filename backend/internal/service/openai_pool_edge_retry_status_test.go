package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// poolModeTestAccount 构造一个开启软冷却、可配置重试次数的池模式 OpenAI 账号。
func poolModeTestAccount(id int64, retryCount int) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                          true,
			"pool_soft_cooldown_enabled":         true,
			"pool_soft_cooldown_error_threshold": 1,
			"pool_mode_retry_count":              retryCount,
		},
	}
}

// edgeRetryTransientStatuses 是 edge 路径应当先“同账号重试、重试耗尽后才软冷却”的瞬时上游状态码。
// 这些状态码必须同时满足两个条件，缺一不可：
//  1. 通过 edge 入口闸门 OpenAIEdgeHTTPStatusRetryable（否则 edge 会在闸门处直接 fallback_go，根本走不到分类器）。
//  2. 被统一分类器 OpenAIPoolFailoverRetryableOnSameAccount 判定为可同账号重试（否则会绕过 retry loop 直接软冷却）。
var edgeRetryTransientStatuses = []int{
	http.StatusUnauthorized,        // 401
	http.StatusForbidden,           // 403
	http.StatusRequestTimeout,      // 408
	http.StatusTooManyRequests,     // 429
	http.StatusInternalServerError, // 500
	http.StatusBadGateway,          // 502
	http.StatusServiceUnavailable,  // 503
	http.StatusGatewayTimeout,      // 504
	529,
}

// TestOpenAIEdgeRetryGateAndClassifierAgree 回归点：edge 入口闸门与统一分类器必须对同一批瞬时状态码达成一致。
//
// 历史 bug：edge 路径用 IsPoolModeRetryableStatus（默认仅 401/403/429）判定同账号重试，
// 而入口闸门放行的范围更宽（含 5xx/超时），导致 500/502/503/504/529 通过闸门后被判为不可重试，
// 直接软冷却、绕过账号配置的重试次数。
//
// 此测试锁定二者的交集，未来任一侧被改窄都会失败。
func TestOpenAIEdgeRetryGateAndClassifierAgree(t *testing.T) {
	account := poolModeTestAccount(700, 3)
	for _, status := range edgeRetryTransientStatuses {
		require.Truef(t, OpenAIEdgeHTTPStatusRetryable(status),
			"status %d must pass the edge ingress gate, otherwise edge falls back before retrying", status)
		require.Truef(t, OpenAIPoolFailoverRetryableOnSameAccount(account, status, "", nil),
			"status %d must be classified retryable-on-same-account, otherwise edge cooldowns without retrying", status)
	}
}

// TestOpenAIPoolFailoverRetryableOnSameAccount_NonPoolAccount 非池模式账号不应进入同账号重试。
func TestOpenAIPoolFailoverRetryableOnSameAccount_NonPoolAccount(t *testing.T) {
	account := &Account{
		ID:          702,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{},
	}
	require.False(t, OpenAIPoolFailoverRetryableOnSameAccount(account, http.StatusInternalServerError, "", nil))
}

// TestOpenAIEdgeRetryThenCooldownBehavior 端到端语义回归：模拟 edge handler 的重试决策循环，
// 断言对瞬时错误“先按账号配置的次数同账号重试，最后一次仍失败后才软冷却”。
//
// 这里复刻 openAIEdgeRetryDecision 中的判定顺序（闸门 → 分类器 → 计数 < 配置 → 重试；
// 否则 HandleOpenAIAccountFailoverSwitch 软冷却），用真实的 service 方法验证软冷却仅在
// 重试耗尽后才被触发，从而同时防住入口闸门、retry loop、软冷却三处被改坏。
func TestOpenAIEdgeRetryThenCooldownBehavior(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError, // 500：历史 bug 直接软冷却的典型状态码
		http.StatusServiceUnavailable,  // 503
		http.StatusTooManyRequests,     // 429：本就在默认列表，作为对照
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const retryLimit = 3
			svc := &OpenAIGatewayService{}
			account := poolModeTestAccount(710, retryLimit)
			ctx := context.Background()
			failoverErr := &UpstreamFailoverError{
				StatusCode:             status,
				RetryableOnSameAccount: OpenAIPoolFailoverRetryableOnSameAccount(account, status, "", nil),
			}
			require.True(t, OpenAIEdgeHTTPStatusRetryable(status), "precondition: status passes edge gate")
			require.True(t, failoverErr.RetryableOnSameAccount, "precondition: status is retryable on same account")

			sameAccountRetries := 0
			// 模拟该账号上连续 retryLimit+2 次失败的上游响应。
			for attempt := 1; attempt <= retryLimit+2; attempt++ {
				if failoverErr.RetryableOnSameAccount && sameAccountRetries < account.GetPoolModeRetryCount() {
					sameAccountRetries++
					_, cooling := svc.openAIPoolAccountSoftCooldownUntil(account)
					require.Falsef(t, cooling,
						"status %d: must NOT be in soft cooldown during retry attempt %d (retry %d/%d)",
						status, attempt, sameAccountRetries, retryLimit)
					continue
				}
				// 重试已耗尽：edge handler 在此调用 failover switch，应触发软冷却。
				svc.HandleOpenAIAccountFailoverSwitch(ctx, nil, "", account, failoverErr)
				break
			}

			require.Equalf(t, retryLimit, sameAccountRetries,
				"status %d: should retry exactly the per-account configured count before cooldown", status)
			_, cooling := svc.openAIPoolAccountSoftCooldownUntil(account)
			require.Truef(t, cooling, "status %d: should be in soft cooldown only after retries exhausted", status)
		})
	}
}

// TestOpenAIEdgeRetryRespectsPerAccountCount 不同账号各自按自己的 pool_mode_retry_count 重试，互不影响。
func TestOpenAIEdgeRetryRespectsPerAccountCount(t *testing.T) {
	for _, retryLimit := range []int{0, 1, 5} {
		account := poolModeTestAccount(720, retryLimit)
		require.Equalf(t, retryLimit, account.GetPoolModeRetryCount(),
			"account should retry exactly its configured count %d", retryLimit)

		status := http.StatusInternalServerError
		retryable := OpenAIPoolFailoverRetryableOnSameAccount(account, status, "", nil)
		retries := 0
		for attempt := 1; attempt <= retryLimit+2; attempt++ {
			if retryable && retries < account.GetPoolModeRetryCount() {
				retries++
				continue
			}
			break
		}
		require.Equalf(t, retryLimit, retries,
			"retry count %d should be honored exactly before falling through to cooldown", retryLimit)
	}
}
