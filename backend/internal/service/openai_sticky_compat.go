package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
)

type openAILegacySessionHashContextKey struct{}

var openAILegacySessionHashKey = openAILegacySessionHashContextKey{}

var (
	openAIStickyLegacyReadFallbackTotal atomic.Int64
	openAIStickyLegacyReadFallbackHit   atomic.Int64
	openAIStickyLegacyDualWriteTotal    atomic.Int64
)

func openAIStickyCompatStats() (legacyReadFallbackTotal, legacyReadFallbackHit, legacyDualWriteTotal int64) {
	return openAIStickyLegacyReadFallbackTotal.Load(),
		openAIStickyLegacyReadFallbackHit.Load(),
		openAIStickyLegacyDualWriteTotal.Load()
}

// DeriveSessionHashFromSeed computes the current-format sticky-session hash
// from an arbitrary seed string.
func DeriveSessionHashFromSeed(seed string) string {
	currentHash, _ := deriveOpenAISessionHashes(seed)
	return currentHash
}

func deriveOpenAISessionHashes(sessionID string) (currentHash string, legacyHash string) {
	normalized := strings.TrimSpace(sessionID)
	if normalized == "" {
		return "", ""
	}

	currentHash = fmt.Sprintf("%016x", xxhash.Sum64String(normalized))
	sum := sha256.Sum256([]byte(normalized))
	legacyHash = hex.EncodeToString(sum[:])
	return currentHash, legacyHash
}

func withOpenAILegacySessionHash(ctx context.Context, legacyHash string) context.Context {
	if ctx == nil {
		return nil
	}
	trimmed := strings.TrimSpace(legacyHash)
	if trimmed == "" {
		return ctx
	}
	return context.WithValue(ctx, openAILegacySessionHashKey, trimmed)
}

func openAILegacySessionHashFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(openAILegacySessionHashKey).(string)
	return strings.TrimSpace(value)
}

func attachOpenAILegacySessionHashToGin(c *gin.Context, legacyHash string) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(withOpenAILegacySessionHash(c.Request.Context(), legacyHash))
}

func (s *OpenAIGatewayService) openAISessionHashReadOldFallbackEnabled() bool {
	if s == nil || s.cfg == nil {
		return true
	}
	return s.cfg.Gateway.OpenAIWS.SessionHashReadOldFallback
}

func (s *OpenAIGatewayService) openAISessionHashDualWriteOldEnabled() bool {
	if s == nil || s.cfg == nil {
		return true
	}
	return s.cfg.Gateway.OpenAIWS.SessionHashDualWriteOld
}

func (s *OpenAIGatewayService) openAISessionCacheKey(sessionHash string) string {
	normalized := strings.TrimSpace(sessionHash)
	if normalized == "" {
		return ""
	}
	return "openai:" + normalized
}

func (s *OpenAIGatewayService) openAILegacySessionCacheKey(ctx context.Context, sessionHash string) string {
	legacyHash := openAILegacySessionHashFromContext(ctx)
	if legacyHash == "" {
		return ""
	}
	legacyKey := "openai:" + legacyHash
	if legacyKey == s.openAISessionCacheKey(sessionHash) {
		return ""
	}
	return legacyKey
}

func (s *OpenAIGatewayService) openAIStickyLegacyTTL(ttl time.Duration) time.Duration {
	legacyTTL := ttl
	if legacyTTL <= 0 {
		legacyTTL = openaiStickySessionTTL
	}
	if legacyTTL > 10*time.Minute {
		return 10 * time.Minute
	}
	return legacyTTL
}

func (s *OpenAIGatewayService) getStickySessionAccountID(ctx context.Context, groupID *int64, sessionHash string) (int64, error) {
	if s == nil || s.cache == nil {
		return 0, nil
	}
	cacheCtx, cancel := withStickySessionCacheTimeout(ctx)
	defer cancel()

	primaryKey := s.openAISessionCacheKey(sessionHash)
	if primaryKey == "" {
		return 0, nil
	}

	accountID, err := s.cache.GetSessionAccountID(cacheCtx, derefGroupID(groupID), primaryKey)
	if err == nil && accountID > 0 {
		return accountID, nil
	}
	if !s.openAISessionHashReadOldFallbackEnabled() {
		return accountID, err
	}

	legacyKey := s.openAILegacySessionCacheKey(ctx, sessionHash)
	if legacyKey == "" {
		return accountID, err
	}

	openAIStickyLegacyReadFallbackTotal.Add(1)
	legacyAccountID, legacyErr := s.cache.GetSessionAccountID(cacheCtx, derefGroupID(groupID), legacyKey)
	if legacyErr == nil && legacyAccountID > 0 {
		openAIStickyLegacyReadFallbackHit.Add(1)
		return legacyAccountID, nil
	}
	return accountID, err
}

func (s *OpenAIGatewayService) setStickySessionAccountID(ctx context.Context, groupID *int64, sessionHash string, accountID int64, ttl time.Duration) error {
	if s == nil || s.cache == nil || accountID <= 0 {
		return nil
	}
	if IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash) {
		if !s.isOpenAIPromptCacheBoostAffinityAccountBindable(ctx, sessionHash, accountID) {
			return nil
		}
	}
	primaryKey := s.openAISessionCacheKey(sessionHash)
	if primaryKey == "" {
		return nil
	}

	cacheCtx, cancel := withStickySessionCacheTimeout(ctx)
	defer cancel()
	start := time.Now()
	err := s.cache.SetSessionAccountID(cacheCtx, derefGroupID(groupID), primaryKey, accountID, ttl)
	recordStickySessionCacheWrite(start, err)
	if err != nil {
		return err
	}

	if !s.openAISessionHashDualWriteOldEnabled() {
		return nil
	}
	legacyKey := s.openAILegacySessionCacheKey(ctx, sessionHash)
	if legacyKey == "" {
		return nil
	}
	start = time.Now()
	err = s.cache.SetSessionAccountID(cacheCtx, derefGroupID(groupID), legacyKey, accountID, s.openAIStickyLegacyTTL(ttl))
	recordStickySessionCacheWrite(start, err)
	if err != nil {
		return err
	}
	openAIStickyLegacyDualWriteTotal.Add(1)
	return nil
}

type openAIStickySessionClaimer interface {
	SetSessionAccountIDIfAbsent(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) (bool, error)
}

// claimStickySessionAccountID only claims a missing primary binding. Explicit
// failover and recovery paths continue using setStickySessionAccountID so they
// can intentionally replace a stale account binding.
func (s *OpenAIGatewayService) claimStickySessionAccountID(ctx context.Context, groupID *int64, sessionHash string, accountID int64, ttl time.Duration) error {
	if s == nil || s.cache == nil || accountID <= 0 {
		return nil
	}
	if IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash) && !s.isOpenAIPromptCacheBoostAffinityAccountBindable(ctx, sessionHash, accountID) {
		return nil
	}
	primaryKey := s.openAISessionCacheKey(sessionHash)
	if primaryKey == "" {
		return nil
	}
	cacheCtx, cancel := withStickySessionCacheTimeout(ctx)
	defer cancel()
	claimer, ok := s.cache.(openAIStickySessionClaimer)
	if !ok {
		// Preserve the binding even for legacy cache implementations that do not
		// expose an atomic SETNX helper. Production Redis caches implement the
		// interface above, so this compatibility read is off the hot path there.
		if existing, getErr := s.getStickySessionAccountID(cacheCtx, groupID, sessionHash); getErr == nil && existing > 0 {
			return nil
		}
		return s.setStickySessionAccountID(cacheCtx, groupID, sessionHash, accountID, ttl)
	}
	start := time.Now()
	claimed, err := claimer.SetSessionAccountIDIfAbsent(cacheCtx, derefGroupID(groupID), primaryKey, accountID, ttl)
	recordStickySessionCacheWrite(start, err)
	if err != nil || !claimed || !s.openAISessionHashDualWriteOldEnabled() {
		return err
	}
	legacyKey := s.openAILegacySessionCacheKey(ctx, sessionHash)
	if legacyKey == "" {
		return nil
	}
	start = time.Now()
	_, err = claimer.SetSessionAccountIDIfAbsent(cacheCtx, derefGroupID(groupID), legacyKey, accountID, s.openAIStickyLegacyTTL(ttl))
	recordStickySessionCacheWrite(start, err)
	if err == nil {
		openAIStickyLegacyDualWriteTotal.Add(1)
	}
	return err
}

func (s *OpenAIGatewayService) refreshStickySessionTTL(ctx context.Context, groupID *int64, sessionHash string, ttl time.Duration) error {
	if s == nil || s.cache == nil {
		return nil
	}
	primaryKey := s.openAISessionCacheKey(sessionHash)
	if primaryKey == "" {
		return nil
	}

	cacheCtx, cancel := withStickySessionCacheTimeout(ctx)
	defer cancel()
	err := s.cache.RefreshSessionTTL(cacheCtx, derefGroupID(groupID), primaryKey, ttl)
	if !s.openAISessionHashReadOldFallbackEnabled() && !s.openAISessionHashDualWriteOldEnabled() {
		return err
	}

	legacyKey := s.openAILegacySessionCacheKey(ctx, sessionHash)
	if legacyKey != "" {
		_ = s.cache.RefreshSessionTTL(cacheCtx, derefGroupID(groupID), legacyKey, s.openAIStickyLegacyTTL(ttl))
	}
	return err
}

func (s *OpenAIGatewayService) deleteStickySessionAccountID(ctx context.Context, groupID *int64, sessionHash string) error {
	if s == nil || s.cache == nil {
		return nil
	}
	primaryKey := s.openAISessionCacheKey(sessionHash)
	if primaryKey == "" {
		return nil
	}

	cacheCtx, cancel := withStickySessionCacheTimeout(ctx)
	defer cancel()
	err := s.cache.DeleteSessionAccountID(cacheCtx, derefGroupID(groupID), primaryKey)
	if !s.openAISessionHashReadOldFallbackEnabled() && !s.openAISessionHashDualWriteOldEnabled() {
		return err
	}

	legacyKey := s.openAILegacySessionCacheKey(ctx, sessionHash)
	if legacyKey != "" {
		_ = s.cache.DeleteSessionAccountID(cacheCtx, derefGroupID(groupID), legacyKey)
	}
	return err
}
