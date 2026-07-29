package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const passkeyFinishBodyMaxBytes = 64 * 1024

type PasskeyHandler struct {
	passkeys   *service.PasskeyService
	auth       *AuthHandler
	settingSvc *service.SettingService
}

func NewPasskeyHandler(passkeys *service.PasskeyService, auth *AuthHandler, settingSvc *service.SettingService) *PasskeyHandler {
	return &PasskeyHandler{passkeys: passkeys, auth: auth, settingSvc: settingSvc}
}

type passkeyOptionsResponse struct {
	SessionToken string `json:"session_token"`
	Options      any    `json:"options"`
}

type passkeyFinishRequest struct {
	SessionToken string          `json:"session_token" binding:"required"`
	Name         string          `json:"name,omitempty"`
	Credential   json.RawMessage `json:"credential" binding:"required"`
}

type passkeyRenameRequest struct {
	Name string `json:"name" binding:"required"`
}

type passkeyPasswordRequest struct {
	Password string `json:"password"`
}

func bindPasskeyPassword(c *gin.Context) string {
	var req passkeyPasswordRequest
	_ = c.ShouldBindJSON(&req)
	return req.Password
}

func (h *PasskeyHandler) BeginLogin(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	assertion, token, err := h.passkeys.BeginLogin(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, passkeyOptionsResponse{SessionToken: token, Options: assertion})
}

func (h *PasskeyHandler) FinishLogin(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	req, ok := bindPasskeyFinishRequest(c)
	if !ok {
		return
	}
	user, err := h.passkeys.FinishLogin(c.Request.Context(), req.SessionToken, cloneRequestWithJSON(c.Request, req.Credential))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if err = h.auth.ensureBackendModeAllowsUser(c.Request.Context(), user); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	middleware2.SetAuditActor(c, user.ID, user.Email)
	c.Set("auth_method", service.AuditAuthMethodPasskey)
	h.auth.authService.RecordSuccessfulLogin(c.Request.Context(), user.ID)
	h.auth.respondWithTokenPair(c, user)
}

func (h *PasskeyHandler) BeginRegistration(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	creation, token, err := h.passkeys.BeginRegistration(c.Request.Context(), subject.UserID, bindPasskeyPassword(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, passkeyOptionsResponse{SessionToken: token, Options: creation})
}

func (h *PasskeyHandler) FinishRegistration(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	req, valid := bindPasskeyFinishRequest(c)
	if !valid {
		return
	}
	credential, err := h.passkeys.FinishRegistration(c.Request.Context(), subject.UserID, req.SessionToken, req.Name, cloneRequestWithJSON(c.Request, req.Credential))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, credential)
}

func (h *PasskeyHandler) List(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	credentials, err := h.passkeys.List(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, credentials)
}

func (h *PasskeyHandler) Rename(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	subject, credentialID, ok := passkeyMutationTarget(c)
	if !ok {
		return
	}
	var req passkeyRenameRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		response.BadRequest(c, "Passkey name is required")
		return
	}
	if err := h.passkeys.Rename(c.Request.Context(), subject.UserID, credentialID, req.Name); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"success": true})
}

func (h *PasskeyHandler) Delete(c *gin.Context) {
	if !h.requireEnabled(c) {
		return
	}
	subject, credentialID, ok := passkeyMutationTarget(c)
	if !ok {
		return
	}
	if err := h.passkeys.Delete(c.Request.Context(), subject.UserID, credentialID, bindPasskeyPassword(c)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"success": true})
}

func (h *PasskeyHandler) requireEnabled(c *gin.Context) bool {
	if h == nil || h.passkeys == nil || h.settingSvc == nil || !h.passkeys.Enabled() {
		response.ErrorFrom(c, service.ErrPasskeysDisabled)
		return false
	}
	enabled, err := h.settingSvc.PasskeyEnabled(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return false
	}
	if !enabled {
		response.ErrorFrom(c, service.ErrPasskeysDisabled)
	}
	return enabled
}

func bindPasskeyFinishRequest(c *gin.Context) (*passkeyFinishRequest, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, passkeyFinishBodyMaxBytes)
	var req passkeyFinishRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Credential) == 0 {
		response.BadRequest(c, "Invalid passkey response")
		return nil, false
	}
	return &req, true
}

func cloneRequestWithJSON(original *http.Request, payload []byte) *http.Request {
	request := original.Clone(original.Context())
	request.Body = io.NopCloser(bytes.NewReader(payload))
	request.ContentLength = int64(len(payload))
	request.Header = original.Header.Clone()
	request.Header.Set("Content-Type", "application/json")
	return request
}

func passkeyMutationTarget(c *gin.Context) (middleware2.AuthSubject, int64, bool) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return middleware2.AuthSubject{}, 0, false
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid passkey ID")
		return middleware2.AuthSubject{}, 0, false
	}
	return subject, id, true
}
