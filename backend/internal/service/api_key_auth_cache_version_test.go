package service

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAPIKeyService_RejectsV20AuthSnapshotAfterPolicyFieldsAdded(t *testing.T) {
	const legacy = `{
		"snapshot": {
			"version": 20,
			"api_key_id": 1,
			"user_id": 2,
			"group_id": 9,
			"status": "active",
			"user": {
				"id": 2,
				"status": "active",
				"role": "user",
				"balance": 10,
				"concurrency": 3
			},
			"group": {
				"id": 9,
				"name": "openai",
				"platform": "openai",
				"status": "active",
				"subscription_type": "standard",
				"rate_multiplier": 1
			}
		}
	}`

	var entry APIKeyAuthCacheEntry
	if err := json.Unmarshal([]byte(legacy), &entry); err != nil {
		t.Fatalf("unmarshal v20 auth snapshot: %v", err)
	}

	apiKey, ok, err := (&APIKeyService{}).applyAuthCacheEntry(context.Background(), "k-v20", &entry)
	if err != nil {
		t.Fatalf("apply v20 auth snapshot: %v", err)
	}
	if ok || apiKey != nil {
		t.Fatalf("expected v20 auth snapshot to be rejected after cache schema change, got ok=%v apiKey=%#v", ok, apiKey)
	}
}

func TestAPIKeyService_RejectsV10AuthSnapshotWithoutModelsListConfig(t *testing.T) {
	groupID := int64(9)
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry(context.Background(), "k-legacy-models-list", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  10,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:               groupID,
				Name:             "openai",
				Platform:         PlatformOpenAI,
				Status:           StatusActive,
				SubscriptionType: SubscriptionTypeStandard,
				RateMultiplier:   1,
			},
		},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatalf("expected v10 auth snapshot to be rejected after models_list_config was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}
