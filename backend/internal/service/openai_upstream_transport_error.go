package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const openAITransportErrorTempUnschedDuration = 10 * time.Minute

var openAITransportFailoverBody = []byte(`{"error":{"type":"upstream_error","message":"Upstream request failed"}}`)

type openAITransportErrorClass struct {
	Persistent bool
}

var openAIPersistentTransportErrorMarkers = []string{
	"authentication failed",
	"proxy authentication required",
	"connection refused",
	"no route to host",
	"network is unreachable",
	"no such host",
}

func classifyOpenAITransportError(err error) openAITransportErrorClass {
	if err == nil {
		return openAITransportErrorClass{}
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return openAITransportErrorClass{Persistent: true}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return openAITransportErrorClass{Persistent: true}
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range openAIPersistentTransportErrorMarkers {
		if strings.Contains(msg, marker) {
			return openAITransportErrorClass{Persistent: true}
		}
	}
	return openAITransportErrorClass{}
}

func (s *OpenAIGatewayService) handleOpenAIUpstreamTransportError(ctx context.Context, c *gin.Context, account *Account, err error, passthrough bool) error {
	safeErr := sanitizeUpstreamErrorMessageForOps(err.Error())
	if account == nil {
		return &UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: openAITransportFailoverBody,
		}
	}
	setOpsUpstreamError(c, 0, safeErr, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: 0,
		Passthrough:        passthrough,
		Kind:               "request_error",
		Message:            safeErr,
	})

	if errors.Is(err, context.Canceled) {
		return err
	}
	if classifyOpenAITransportError(err).Persistent {
		s.tempUnscheduleOpenAITransportError(ctx, account, safeErr)
	}
	return &UpstreamFailoverError{
		StatusCode:   http.StatusBadGateway,
		ResponseBody: openAITransportFailoverBody,
	}
}

func (s *OpenAIGatewayService) tempUnscheduleOpenAITransportError(ctx context.Context, account *Account, safeErr string) {
	if s == nil || account == nil {
		return
	}
	until := time.Now().Add(openAITransportErrorTempUnschedDuration)
	reason := "upstream transport error (proxy/network): " + safeErr

	s.BlockAccountScheduling(account, until, "transport_error")

	log := logger.L().With(zap.String("component", "service.openai_gateway"))
	if s.accountRepo == nil {
		log.Warn(
			"openai.account_temp_unscheduled_transport_memory_only",
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("platform", account.Platform),
			zap.Time("until", until),
			zap.String("reason", reason),
		)
		return
	}

	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIAccountStateUpdateTimeout)
	defer cancel()
	if err := s.accountRepo.SetTempUnschedulable(bgCtx, account.ID, until, reason); err != nil {
		log.Warn(
			"openai.account_temp_unscheduled_transport_failed",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return
	}

	log.Warn(
		"openai.account_temp_unscheduled_transport",
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.Time("until", until),
		zap.String("reason", reason),
	)
}
