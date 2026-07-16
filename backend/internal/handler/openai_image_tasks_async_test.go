package handler

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestValidatePersistentImageTaskReplay(t *testing.T) {
	headers := map[string][]string{"Content-Type": {"application/json"}}
	task := &service.OpenAIImageTask{Endpoint: "/v1/images/generations", RequestBody: []byte(`{"prompt":"cat"}`), RequestHeaders: headers}
	require.NoError(t, validatePersistentImageTaskReplay(task, task.Endpoint, task.RequestBody, headers))
	require.ErrorIs(t, validatePersistentImageTaskReplay(task, task.Endpoint, []byte(`{"prompt":"dog"}`), headers), errOpenAIImageTaskIdempotencyConflict)
	require.ErrorIs(t, validatePersistentImageTaskReplay(task, task.Endpoint, task.RequestBody, map[string][]string{"OpenAI-Project": {"other"}}), errOpenAIImageTaskIdempotencyConflict)
}

func TestValidatePersistentImageTaskAPIKey(t *testing.T) {
	groupID := int64(20)
	active := func() *service.APIKey {
		return &service.APIKey{
			ID: 1, UserID: 10, GroupID: &groupID, Status: service.StatusAPIKeyActive,
			User:  &service.User{ID: 10, Status: service.StatusActive},
			Group: &service.Group{ID: groupID, Status: service.StatusActive},
		}
	}
	require.NoError(t, validatePersistentImageTaskAPIKey(active()))

	tests := []struct {
		name   string
		mutate func(*service.APIKey)
	}{
		{"disabled key", func(k *service.APIKey) { k.Status = service.StatusAPIKeyDisabled }},
		{"expired status", func(k *service.APIKey) { k.Status = service.StatusAPIKeyExpired }},
		{"runtime expired", func(k *service.APIKey) { expired := time.Now().Add(-time.Minute); k.ExpiresAt = &expired }},
		{"quota status", func(k *service.APIKey) { k.Status = service.StatusAPIKeyQuotaExhausted }},
		{"runtime quota exhausted", func(k *service.APIKey) { k.Quota, k.QuotaUsed = 1, 1 }},
		{"missing user", func(k *service.APIKey) { k.User = nil }},
		{"mismatched user", func(k *service.APIKey) { k.User.ID = 11 }},
		{"inactive user", func(k *service.APIKey) { k.User.Status = service.StatusDisabled }},
		{"missing group", func(k *service.APIKey) { k.Group = nil }},
		{"mismatched group", func(k *service.APIKey) { k.Group.ID = 21 }},
		{"inactive group", func(k *service.APIKey) { k.Group.Status = service.StatusDisabled }},
		{"exclusive group revoked", func(k *service.APIKey) { k.Group.IsExclusive = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := active()
			tt.mutate(key)
			require.Error(t, validatePersistentImageTaskAPIKey(key))
		})
	}

	withoutGroup := active()
	withoutGroup.GroupID = nil
	withoutGroup.Group = nil
	require.NoError(t, validatePersistentImageTaskAPIKey(withoutGroup))

	exclusiveAllowed := active()
	exclusiveAllowed.Group.IsExclusive = true
	exclusiveAllowed.User.AllowedGroups = []int64{groupID}
	require.NoError(t, validatePersistentImageTaskAPIKey(exclusiveAllowed))
}

func TestStandardOpenAIImageTaskIDIsOwnerScopedAndStable(t *testing.T) {
	one := standardOpenAIImageTaskID("api_key:1", "/v1/images/generations", "request-1")
	two := standardOpenAIImageTaskID("api_key:1", "/v1/images/generations", "request-1")
	other := standardOpenAIImageTaskID("api_key:2", "/v1/images/generations", "request-1")
	require.Equal(t, one, two)
	require.NotEqual(t, one, other)
	require.Equal(t, "/v1/images/tasks/"+one, standardOpenAIImageTaskPollURL("/v1/images/generations/async", one))
}

func TestPersistentOpenAIImageTaskUsesConfiguredRetention(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	task := &service.OpenAIImageTask{ID: "task-retention", Model: "gpt-image-1", CreatedAt: now, UpdatedAt: now}
	public := standardPersistentOpenAIImageTaskWithRetention(task, "/v1/images/tasks/task-retention", 6*time.Hour)
	require.Equal(t, now.Add(6*time.Hour).Unix(), public.ExpiresAt)
}

func TestMemoryOpenAIImageTaskUsesConfiguredRetention(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	task := &openAIImageTask{ID: "task-retention", Model: "gpt-image-1", CreatedAt: now, UpdatedAt: now}
	public := standardMemoryOpenAIImageTaskWithRetention(task, "/v1/images/tasks/task-retention", 6*time.Hour)
	require.Equal(t, now.Add(6*time.Hour).Unix(), public.ExpiresAt)
}
