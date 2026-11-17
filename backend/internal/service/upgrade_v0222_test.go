package service

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestV0222BufferedCompatGrokSparseSearchDoesNotEscapeToClient(t *testing.T) {
	const stream = `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","call_id":"xs_search","name":"search"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","input":"{\"query\":\"server query\"}"}}

data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"xs_client","name":"client","arguments":"{}"}}

data: {"type":"response.completed","response":{"id":"resp_fixture","model":"fixture","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16,"input_tokens_details":{"cached_tokens":4}}}}

`
	for _, platform := range []string{PlatformGrok, PlatformOpenAI} {
		for _, dialect := range []string{"chat", "messages"} {
			t.Run(platform+"/"+dialect, func(t *testing.T) {
				setGinTestMode()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+dialect, nil)
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(stream))}
				defer func() { _ = resp.Body.Close() }()
				svc := &OpenAIGatewayService{}
				account := &Account{ID: 1, Platform: platform, Type: AccountTypeAPIKey}
				handle := svc.handleChatBufferedStreamingResponse
				if dialect == "messages" {
					handle = svc.handleAnthropicBufferedStreamingResponse
				}
				result, err := handle(context.Background(), resp, c, account, "public", "fixture", "fixture", time.Now())
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, 11, result.Usage.InputTokens)
				require.Equal(t, 5, result.Usage.OutputTokens)
				require.Equal(t, 4, result.Usage.CacheReadInputTokens)
				require.Equal(t, "response.completed", result.TerminalEventType)
				path := "choices.0.message.tool_calls"
				if dialect == "messages" {
					path = "content"
				}
				calls := gjson.GetBytes(rec.Body.Bytes(), path).Array()
				if platform == PlatformGrok {
					require.Len(t, calls, 1)
					require.Equal(t, "xs_client", calls[0].Get("id").String())
				} else {
					require.Len(t, calls, 2, rec.Body.String())
					require.Equal(t, "xs_search", calls[0].Get("id").String())
				}
			})
		}
	}
}

type v0222QuotaSnapshotRepo struct {
	AccountRepository
	updates map[string]any
}

func (r *v0222QuotaSnapshotRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updates = updates
	return nil
}

type v0222QuotaSnapshotHTTP struct {
	HTTPUpstream
	body string
}

func (h *v0222QuotaSnapshotHTTP) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(h.body))}, nil
}

func TestV0222QuotaProbeOnlyClearsTrustedKimiSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name, provider, body string
		success, clearWeekly bool
	}{
		{"invalid legacy weekly", PlatformKimi, `{"limits":[{"detail":{"limit":100,"remaining":50}}],"usage":{}}`, false, false},
		{"invalid legacy 5h", PlatformKimi, `{"limits":[{"detail":{}}],"usages":{"limit_month_code":{"used_ratio":0}}}`, false, false},
		{"invalid new container", PlatformKimi, `{"limits":[{"detail":{"limit":100,"remaining":50}}],"usages":null}`, false, false},
		{"complete Kimi snapshot", PlatformKimi, `{"usages":{"limit_5h":{"used_ratio":0}}}`, true, true},
		{"partial Zhipu snapshot", PlatformZhipu, `{"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":0},{"type":"TOKENS_LIMIT","unit":6}]}}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &v0222QuotaSnapshotRepo{}
			s := NewCNProviderQuotaService(repo, nil, &v0222QuotaSnapshotHTTP{body: tc.body}, nil)
			account := &Account{ID: 1, Platform: tc.provider, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture", "account_mode": "coding"}}
			result, err := s.queryUsageForAccount(context.Background(), account)
			require.NoError(t, err)
			require.Equal(t, tc.success, result.Success)
			if !tc.success {
				require.False(t, result.Persisted)
				require.Nil(t, repo.updates, "failed observations must not touch the last good snapshot")
				return
			}
			require.True(t, result.Persisted)
			value, present := repo.updates[tc.provider+"_weekly_used_percent"]
			require.Equal(t, tc.clearWeekly, present)
			if present {
				require.Nil(t, value)
			}
		})
	}
}

func TestV0222KimiMonthlyAndFiniteObservations(t *testing.T) {
	tiers := parseKimiUsageTiers([]byte(`{"usages":{"limit_5h":{"used_ratio":0},"limit_month_total":{"used_ratio":0.9,"reset_time":"2026-11-26T00:00:00Z"},"limit_month_code":{"used_ratio":1,"reset_time":"2026-11-20T00:00:00Z"}}}`))
	require.Len(t, tiers, 2)
	require.Equal(t, 0.0, tiers[0].UsedPercent)
	require.Equal(t, 100.0, tiers[1].UsedPercent)
	require.Equal(t, "2026-11-26T00:00:00Z", tiers[1].ResetAt)
	for _, body := range []string{`{"limits":[{"detail":{}}]}`, `{"usages":{"limit_5h":{"used_ratio":"NaN"}}}`, `{"usages":{"limit_month_total":{"used_ratio":65}}}`} {
		require.Empty(t, parseKimiUsageTiers([]byte(body)))
	}
	updates := cnQuotaExtraUpdates(PlatformKimi, tiers, time.Now())
	clearAbsentCNQuotaWindows(PlatformKimi, tiers, updates)
	require.Contains(t, updates, "kimi_weekly_used_percent")
	require.Nil(t, updates["kimi_weekly_used_percent"])
}

func TestV0222OpenAIQuotaUsesObservationTime(t *testing.T) {
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	usage := buildOpenAIUsageFromExtra(&Account{Extra: map[string]any{"codex_5h_used_percent": 0, "codex_usage_updated_at": now.Add(-time.Hour).Format(time.RFC3339)}}, now)
	require.Equal(t, now.Add(-time.Hour), *usage.UpdatedAt)
	require.Equal(t, "passive", usage.Source)
	require.NotNil(t, usage.FiveHour)
	require.Zero(t, usage.FiveHour.Utilization)
	require.Nil(t, buildOpenAIUsageFromExtra(&Account{Extra: map[string]any{"codex_5h_used_percent": nil}}, now).FiveHour)
	require.Nil(t, buildOpenAIUsageFromExtra(&Account{}, now).UpdatedAt)
	require.False(t, hasOpenAIQuotaObservation(map[string]any{"codex_5h_used_percent": nil}))
	require.False(t, hasOpenAIQuotaObservation(map[string]any{"codex_7d_used_percent": "NaN"}))
}

func TestV0222KimiPartialMalformedWindowCannotClearPreviousQuota(t *testing.T) {
	for _, monthly := range []string{`null`, `{}`, `{"used_ratio":"NaN"}`, `{"used_ratio":2}`} {
		body := []byte(`{"usages":{"limit_5h":{"used_ratio":0},"limit_month_total":` + monthly + `}}`)
		require.NotEmpty(t, parseKimiUsageTiers(body))
		require.False(t, kimiDeclaredQuotaWindowsValid(body))
	}
	require.True(t, kimiDeclaredQuotaWindowsValid([]byte(`{"usages":{"limit_5h":{"used_ratio":0}}}`)))
	require.True(t, kimiDeclaredQuotaWindowsValid([]byte(`{"usages":{"limit_month_code":{"used_ratio":0}}}`)))
}

func TestV0222KimiLegacyPartialWindowsMustBeValidBeforeClearing(t *testing.T) {
	for _, legacy := range []string{
		`"usage":null`, `"usage":{}`, `"usage":{"limit":100,"remaining":"NaN"}`,
		`"usage":{"limit":100,"remaining":101}`, `"limits":null`, `"limits":{}`,
		`"limits":[{"detail":{}}]`, `"limits":[{"detail":{"limit":0,"remaining":0}}]`,
		`"limits":[{"detail":{"limit":100,"remaining":50}}, {"detail":{}}]`,
	} {
		t.Run(legacy, func(t *testing.T) {
			body := []byte(`{"usages":{"limit_month_code":{"used_ratio":0}},` + legacy + `}`)
			require.NotEmpty(t, parseKimiUsageTiers(body))
			require.False(t, kimiDeclaredQuotaWindowsValid(body))
		})
	}
	for _, usages := range []string{`null`, `[]`, `"invalid"`} {
		require.False(t, kimiDeclaredQuotaWindowsValid([]byte(`{"limits":[{"detail":{"limit":100,"remaining":0}}],"usages":`+usages+`}`)))
	}
	require.True(t, kimiDeclaredQuotaWindowsValid([]byte(`{"limits":[{"detail":{"limit":100,"remaining":0}}],"usage":{"limit":"1000","remaining":"700"}}`)))
	require.True(t, kimiDeclaredQuotaWindowsValid([]byte(`{"limits":[],"usages":{"limit_5h":{"used_ratio":0}}}`)))
}

func TestV0222PartialQuotaUpdatesKeepOtherProviderWindows(t *testing.T) {
	for _, provider := range []string{PlatformZhipu, PlatformMiniMax, PlatformOpenCodeGo} {
		t.Run(provider, func(t *testing.T) {
			tiers := []CNQuotaTier{{Window: "5h", UsedPercent: 0}}
			updates := cnQuotaExtraUpdates(provider, tiers, time.Now())
			clearAbsentCNQuotaWindows(provider, tiers, updates)
			require.Equal(t, 0.0, updates[provider+"_5h_used_percent"])
			require.NotContains(t, updates, provider+"_weekly_used_percent")
			require.NotContains(t, updates, provider+"_weekly_reset_at")
			require.NotContains(t, updates, provider+"_5h_reset_at")
		})
	}
}

func TestV0222AdditionalToolIdentityPreservesOpaqueCalls(t *testing.T) {
	const id = "at_12345678-1234-1234-1234-123456789abc"
	body := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"additional_tools","id":"` + id + `","tools":[{"type":"namespace","name":"shell"}]},{"type":"item_reference","id":"` + id + `"},{"type":"function_call","id":"fc_keep","call_id":"opaque_keep","encrypted_content":"sealed"}]}`)
	out, changed, err := normalizeCodexAdditionalToolsIDsBody(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "at_12345678123412341234123456789abc", gjson.GetBytes(out, "input.0.id").String())
	require.Equal(t, gjson.GetBytes(out, "input.0.id").String(), gjson.GetBytes(out, "input.1.id").String())
	require.Equal(t, gjson.GetBytes(body, "input.2").Raw, gjson.GetBytes(out, "input.2").Raw)
	var parsed struct {
		Input []any `json:"input"`
	}
	require.NoError(t, json.Unmarshal(body, &parsed))
	filtered := filterCodexInputWithOptions(parsed.Input, codexInputFilterOptions{PreserveCallIDs: true})
	declaration, ok := filtered[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "at_12345678123412341234123456789abc", declaration["id"])
	require.Len(t, filtered, 3)
	original, ok := parsed.Input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, id, original["id"])
}

func TestV0222PerTurnControlBetaPreservesMessageConfig(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":[],"output_config":{"effort":"high"}}]}`)
	out, changed := stripAnthropicMessageOutputConfigUnlessBeta(body, "per-turn-control-2026-07-01")
	require.False(t, changed)
	require.Equal(t, body, out)
}
