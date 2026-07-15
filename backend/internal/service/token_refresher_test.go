//go:build unit

package service

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClaudeTokenRefresher_NeedsRefresh(t *testing.T) {
	refresher := &ClaudeTokenRefresher{}
	refreshWindow := 30 * time.Minute

	tests := []struct {
		name        string
		credentials map[string]any
		wantRefresh bool
	}{
		{
			name: "expires_at as string - expired",
			credentials: map[string]any{
				"expires_at": "1000", // 1970-01-01 00:16:40 UTC, 已过期
			},
			wantRefresh: true,
		},
		{
			name: "expires_at as float64 - expired",
			credentials: map[string]any{
				"expires_at": float64(1000), // 数字类型，已过期
			},
			wantRefresh: true,
		},
		{
			name: "expires_at as RFC3339 - expired",
			credentials: map[string]any{
				"expires_at": "1970-01-01T00:00:00Z", // RFC3339 格式，已过期
			},
			wantRefresh: true,
		},
		{
			name: "expires_at as string - far future",
			credentials: map[string]any{
				"expires_at": "9999999999", // 远未来
			},
			wantRefresh: false,
		},
		{
			name: "expires_at as float64 - far future",
			credentials: map[string]any{
				"expires_at": float64(9999999999), // 远未来，数字类型
			},
			wantRefresh: false,
		},
		{
			name: "expires_at as RFC3339 - far future",
			credentials: map[string]any{
				"expires_at": "2099-12-31T23:59:59Z", // RFC3339 格式，远未来
			},
			wantRefresh: false,
		},
		{
			name:        "expires_at missing",
			credentials: map[string]any{},
			wantRefresh: false,
		},
		{
			name: "expires_at is nil",
			credentials: map[string]any{
				"expires_at": nil,
			},
			wantRefresh: false,
		},
		{
			name: "expires_at is invalid string",
			credentials: map[string]any{
				"expires_at": "invalid",
			},
			wantRefresh: false,
		},
		{
			name:        "credentials is nil",
			credentials: nil,
			wantRefresh: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform:    PlatformAnthropic,
				Type:        AccountTypeOAuth,
				Credentials: tt.credentials,
			}

			got := refresher.NeedsRefresh(account, refreshWindow)
			require.Equal(t, tt.wantRefresh, got)
		})
	}
}

func TestOpenAITokenRefresher_NeedsRefresh_ClampsBackgroundWindow(t *testing.T) {
	refresher := &OpenAITokenRefresher{}

	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "rt",
			"expires_at":    time.Now().Add(10 * time.Minute).Unix(),
		},
	}

	require.False(t, refresher.NeedsRefresh(account, 30*time.Minute), "OpenAI should not use the generic 30m background refresh window")

	account.Credentials["expires_at"] = time.Now().Add(2 * time.Minute).Unix()
	require.True(t, refresher.NeedsRefresh(account, 30*time.Minute), "OpenAI should refresh inside its 3m background window")
}

func TestClaudeTokenRefresher_NeedsRefresh_WithinWindow(t *testing.T) {
	refresher := &ClaudeTokenRefresher{}
	refreshWindow := 30 * time.Minute

	// 设置一个在刷新窗口内的时间（当前时间 + 15分钟）
	expiresAt := time.Now().Add(15 * time.Minute).Unix()

	tests := []struct {
		name        string
		credentials map[string]any
	}{
		{
			name: "string type - within refresh window",
			credentials: map[string]any{
				"expires_at": strconv.FormatInt(expiresAt, 10),
			},
		},
		{
			name: "float64 type - within refresh window",
			credentials: map[string]any{
				"expires_at": float64(expiresAt),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform:    PlatformAnthropic,
				Type:        AccountTypeOAuth,
				Credentials: tt.credentials,
			}

			got := refresher.NeedsRefresh(account, refreshWindow)
			require.True(t, got, "should need refresh when within window")
		})
	}
}

func TestClaudeTokenRefresher_NeedsRefresh_OutsideWindow(t *testing.T) {
	refresher := &ClaudeTokenRefresher{}
	refreshWindow := 30 * time.Minute

	// 设置一个在刷新窗口外的时间（当前时间 + 1小时）
	expiresAt := time.Now().Add(1 * time.Hour).Unix()

	tests := []struct {
		name        string
		credentials map[string]any
	}{
		{
			name: "string type - outside refresh window",
			credentials: map[string]any{
				"expires_at": strconv.FormatInt(expiresAt, 10),
			},
		},
		{
			name: "float64 type - outside refresh window",
			credentials: map[string]any{
				"expires_at": float64(expiresAt),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform:    PlatformAnthropic,
				Type:        AccountTypeOAuth,
				Credentials: tt.credentials,
			}

			got := refresher.NeedsRefresh(account, refreshWindow)
			require.False(t, got, "should not need refresh when outside window")
		})
	}
}

func TestClaudeTokenRefresher_CanRefresh(t *testing.T) {
	refresher := &ClaudeTokenRefresher{}

	tests := []struct {
		name     string
		platform string
		accType  string
		shadow   bool
		want     bool
	}{
		{
			name:     "anthropic oauth - can refresh",
			platform: PlatformAnthropic,
			accType:  AccountTypeOAuth,
			want:     true,
		},
		{
			name:     "anthropic api-key - cannot refresh",
			platform: PlatformAnthropic,
			accType:  AccountTypeAPIKey,
			want:     false,
		},
		{
			name:     "openai oauth - cannot refresh",
			platform: PlatformOpenAI,
			accType:  AccountTypeOAuth,
			want:     false,
		},
		{
			name:     "gemini oauth - cannot refresh",
			platform: PlatformGemini,
			accType:  AccountTypeOAuth,
			want:     false,
		},
		{
			name:     "anthropic shadow oauth - cannot refresh",
			platform: PlatformAnthropic,
			accType:  AccountTypeOAuth,
			shadow:   true,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var parentID *int64
			if tt.shadow {
				id := int64(1)
				parentID = &id
			}
			account := &Account{
				Platform:        tt.platform,
				Type:            tt.accType,
				ParentAccountID: parentID,
			}

			got := refresher.CanRefresh(account)
			require.Equal(t, tt.want, got)
		})
	}
	require.False(t, refresher.CanRefresh(nil))
}

func TestOpenAITokenRefresher_CanRefresh(t *testing.T) {
	refresher := &OpenAITokenRefresher{}

	tests := []struct {
		name     string
		platform string
		accType  string
		shadow   bool
		want     bool
	}{
		{
			name:     "openai oauth - can refresh",
			platform: PlatformOpenAI,
			accType:  AccountTypeOAuth,
			want:     true,
		},
		{
			name:     "openai apikey - cannot refresh",
			platform: PlatformOpenAI,
			accType:  AccountTypeAPIKey,
			want:     false,
		},
		{
			name:     "openai shadow oauth - cannot refresh",
			platform: PlatformOpenAI,
			accType:  AccountTypeOAuth,
			shadow:   true,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var parentID *int64
			if tt.shadow {
				id := int64(1)
				parentID = &id
			}
			account := &Account{
				Platform:        tt.platform,
				Type:            tt.accType,
				ParentAccountID: parentID,
			}
			require.Equal(t, tt.want, refresher.CanRefresh(account))
		})
	}
	require.False(t, refresher.CanRefresh(nil))
}

func TestPlatformTokenRefreshers_CanRefreshRejectShadowAndNil(t *testing.T) {
	parentID := int64(1)
	tests := []struct {
		name      string
		refresher TokenRefresher
		account   *Account
		want      bool
	}{
		{
			name:      "gemini oauth",
			refresher: &GeminiTokenRefresher{},
			account:   &Account{Platform: PlatformGemini, Type: AccountTypeOAuth},
			want:      true,
		},
		{
			name:      "gemini shadow",
			refresher: &GeminiTokenRefresher{},
			account:   &Account{Platform: PlatformGemini, Type: AccountTypeOAuth, ParentAccountID: &parentID},
			want:      false,
		},
		{
			name:      "grok oauth",
			refresher: &GrokTokenRefresher{},
			account: &Account{
				Platform: PlatformGrok,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"refresh_token": "grok-refresh-token",
				},
			},
			want: true,
		},
		{
			name:      "grok oauth missing refresh token",
			refresher: &GrokTokenRefresher{},
			account:   &Account{Platform: PlatformGrok, Type: AccountTypeOAuth},
			want:      false,
		},
		{
			name:      "grok shadow",
			refresher: &GrokTokenRefresher{},
			account:   &Account{Platform: PlatformGrok, Type: AccountTypeOAuth, ParentAccountID: &parentID},
			want:      false,
		},
		{
			name:      "antigravity oauth",
			refresher: &AntigravityTokenRefresher{},
			account:   &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth},
			want:      true,
		},
		{
			name:      "antigravity shadow",
			refresher: &AntigravityTokenRefresher{},
			account:   &Account{Platform: PlatformAntigravity, Type: AccountTypeOAuth, ParentAccountID: &parentID},
			want:      false,
		},
		{
			name:      "nil account",
			refresher: &GeminiTokenRefresher{},
			account:   nil,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.refresher.CanRefresh(tt.account))
		})
	}
}
