package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type cacheInvalidationGenerationRepo struct {
	UserSubscriptionRepository
	sub *UserSubscription
	key *APIKeyRateLimitData
}

func (r *cacheInvalidationGenerationRepo) GetByUserIDAndGroupID(context.Context, int64, int64) (*UserSubscription, error) {
	return r.sub, nil
}
func (r *cacheInvalidationGenerationRepo) GetRateLimitData(context.Context, int64) (*APIKeyRateLimitData, error) {
	return r.key, nil
}

type cacheInvalidationGenerationCache struct {
	billingCacheWorkerStub
	owners      []int64
	generations []int64
}

func (c *cacheInvalidationGenerationCache) InvalidateSubscriptionCacheWithGeneration(_ context.Context, userID, groupID, generation int64) error {
	c.owners = append(c.owners, userID, groupID)
	c.generations = append(c.generations, generation)
	return nil
}
func (c *cacheInvalidationGenerationCache) InvalidateAPIKeyRateLimitWithGeneration(_ context.Context, keyID, generation int64) error {
	c.owners = append(c.owners, keyID)
	c.generations = append(c.generations, generation)
	return nil
}

func TestGeminiCacheInvalidationReadsCommittedGeneration(t *testing.T) {
	r := &cacheInvalidationGenerationRepo{sub: &UserSubscription{GeminiCacheGeneration: 4}, key: &APIKeyRateLimitData{GeminiCacheGeneration: 7}}
	c := &cacheInvalidationGenerationCache{}
	s := &BillingCacheService{cache: c, subRepo: r, apiKeyRateLimitLoader: r}
	require.NoError(t, s.InvalidateSubscription(t.Context(), 1, 2))
	require.NoError(t, s.InvalidateAPIKeyRateLimit(t.Context(), 3))
	require.Equal(t, []int64{1, 2, 3}, c.owners)
	require.Equal(t, []int64{4, 7}, c.generations, "fence uses the post-reset DB generation")
	// Ordinary owners keep generation zero and do not invent cache fences.
	r.key.GeminiCacheGeneration = 0
	require.NoError(t, s.InvalidateAPIKeyRateLimit(t.Context(), 3))
	require.Zero(t, c.generations[2])
}
