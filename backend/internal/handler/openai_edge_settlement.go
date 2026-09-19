package handler

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// This encrypted, authenticated capsule travels only on the internal control
// channel and in Edge's settlement WAL. It contains no prompt or response text.
// Keeping the billing snapshot with the callback survives Go restarts and
// wrong-node delivery without requiring the original process-local lease.
type openAIEdgeSettlement struct {
	Version                                                     int
	LeaseID, RequestID, AttemptID                               string
	BillingRequestID                                            string
	APIKey                                                      *service.APIKey
	Account                                                     *service.Account
	Subscription                                                *service.UserSubscription
	QuotaPlatform, Model, BillingModel, UpstreamModel           string
	ReasoningEffort, ServiceTier                                *string
	RequestedReasoningEffort, InboundEndpoint, UpstreamEndpoint string
	UserAgent, ClientIP, PayloadHash, SessionID, SessionHash    string
	Channel                                                     service.ChannelUsageFields
}

func edgeSettlementSnapshot(l *openAIEdgeLease) openAIEdgeSettlement {
	return openAIEdgeSettlement{
		Version: 1, LeaseID: l.leaseID, RequestID: l.edgeRequestID, AttemptID: l.currentAttemptID,
		BillingRequestID: l.settlementBillingID,
		APIKey:           l.apiKey, Account: l.account, Subscription: l.subscription,
		QuotaPlatform: l.quotaPlatform, Model: l.requestModel, BillingModel: l.billingModel, UpstreamModel: l.upstreamModel,
		ReasoningEffort: l.reasoningEffort, ServiceTier: l.serviceTier, RequestedReasoningEffort: l.requestedReasoningEffort,
		InboundEndpoint: l.inboundEndpoint, UpstreamEndpoint: l.upstreamEndpoint,
		UserAgent: l.userAgent, ClientIP: l.clientIP, PayloadHash: l.requestPayloadHash,
		SessionID: l.sessionID, SessionHash: l.sessionHash, Channel: l.channelUsageFields,
	}
}

func (h *OpenAIGatewayHandler) edgeSettlementCipher() (cipher.AEAD, error) {
	secret := strings.TrimSpace(h.openAIEdgeConfig().InternalSecret)
	if secret == "" {
		return nil, errors.New("edge settlement secret missing")
	}
	key := sha256.Sum256([]byte("sub2api.edge.settlement.v1\x00" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (h *OpenAIGatewayHandler) sealEdgeSettlement(s openAIEdgeSettlement) (string, error) {
	// Avoid recursive associations and unrelated user authentication material.
	if s.APIKey == nil || s.Account == nil || s.APIKey.User == nil {
		return "", errors.New("edge settlement identity missing")
	}
	k := *s.APIKey
	u := *k.User
	a := *s.Account
	u.PasswordHash = ""
	u.TotpSecretEncrypted = nil
	u.APIKeys = nil
	u.Subscriptions = nil
	k.User = &u
	a.Proxy = nil
	a.Groups = nil
	a.AccountGroups = nil
	s.APIKey = &k
	s.Account = &a
	if s.Subscription != nil {
		sub := *s.Subscription
		sub.User = nil
		sub.AssignedByUser = nil
		sub.Group = nil
		s.Subscription = &sub
	}
	data, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	if len(data) > 2<<20 {
		return "", errors.New("edge settlement context too large")
	}
	aead, err := h.edgeSettlementCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, data, nil)), nil
}

func (h *OpenAIGatewayHandler) restoreEdgeSettlement(req service.OpenAIEdgeCompleteRequest) (*openAIEdgeLease, error) {
	if len(req.SettlementContext) > 3<<20 {
		return nil, errors.New("invalid settlement context")
	}
	aead, err := h.edgeSettlementCipher()
	if err != nil {
		return nil, err
	}
	data, err := base64.RawURLEncoding.DecodeString(req.SettlementContext)
	if err != nil || len(data) < aead.NonceSize() {
		return nil, errors.New("invalid settlement context")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
	if err != nil {
		return nil, errors.New("invalid settlement context")
	}
	var s openAIEdgeSettlement
	if json.Unmarshal(plain, &s) != nil || s.Version != 1 || s.LeaseID == "" || s.LeaseID != req.LeaseID || s.RequestID != req.EdgeRequestID || s.Account == nil || s.Account.ID != req.AccountID || s.APIKey == nil || s.APIKey.User == nil {
		return nil, errors.New("settlement identity mismatch")
	}
	return &openAIEdgeLease{
		settled: true, leaseID: s.LeaseID, edgeRequestID: s.RequestID, currentAttemptID: s.AttemptID,
		settlementBillingID: s.BillingRequestID,
		apiKey:              s.APIKey, account: s.Account, subscription: s.Subscription, quotaPlatform: s.QuotaPlatform,
		requestModel: s.Model, billingModel: s.BillingModel, upstreamModel: s.UpstreamModel,
		reasoningEffort: s.ReasoningEffort, serviceTier: s.ServiceTier, requestedReasoningEffort: s.RequestedReasoningEffort,
		inboundEndpoint: s.InboundEndpoint, upstreamEndpoint: s.UpstreamEndpoint,
		userAgent: s.UserAgent, clientIP: s.ClientIP, requestPayloadHash: s.PayloadHash,
		sessionID: s.SessionID, sessionHash: s.SessionHash, channelUsageFields: s.Channel,
	}, nil
}
