package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type ScopedAdminHandler struct{ service *service.ScopedAdminService }

func NewScopedAdminHandler(s *service.ScopedAdminService) *ScopedAdminHandler {
	return &ScopedAdminHandler{service: s}
}
func scopedBody(c *gin.Context, out any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10))
	if err != nil {
		response.BadRequest(c, "Request body is too large or invalid")
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		response.BadRequest(c, "Invalid or unsupported fields")
		return false
	}
	return true
}
func (h *ScopedAdminHandler) jwt(c *gin.Context) bool {
	if h == nil || !h.service.Enabled() {
		response.NotFound(c, "Resource not found")
		return false
	}
	if c.GetString("auth_method") != "jwt" {
		response.Forbidden(c, "An administrator session is required")
		return false
	}
	return true
}
func (h *ScopedAdminHandler) Capabilities(c *gin.Context) {
	if c.GetString("auth_method") != "jwt" {
		response.Forbidden(c, "An administrator session is required")
		return
	}
	response.Success(c, gin.H{"enabled": h != nil && h.service.Enabled(), "routes": adminaccess.Routes, "permissions": adminaccess.Permissions})
}
func (h *ScopedAdminHandler) List(c *gin.Context) {
	if !h.jwt(c) {
		return
	}
	var beforeID int64
	var err error
	if raw := c.Query("before_id"); raw != "" {
		beforeID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || beforeID <= 0 {
			response.BadRequest(c, "Invalid key list cursor")
			return
		}
	}
	keys, err := h.service.ListKeys(c.Request.Context(), beforeID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, keys)
}
func (h *ScopedAdminHandler) Create(c *gin.Context) {
	if !h.jwt(c) {
		return
	}
	var in service.ScopedAdminKeyInput
	if !scopedBody(c, &in) {
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Forbidden(c, "An administrator session is required")
		return
	}
	key, secret, err := h.service.CreateKey(c.Request.Context(), subject.UserID, in)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Created(c, gin.H{"key": key, "secret": secret})
}
func (h *ScopedAdminHandler) Update(c *gin.Context) {
	if !h.jwt(c) {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid ID")
		return
	}
	var in service.ScopedAdminKeyInput
	if !scopedBody(c, &in) {
		return
	}
	if err = h.service.UpdateKey(c.Request.Context(), id, in); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"saved": true})
}
func (h *ScopedAdminHandler) Change(c *gin.Context) {
	if !h.jwt(c) {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid ID")
		return
	}
	var in struct {
		Revision int64 `json:"revision"`
	}
	if !scopedBody(c, &in) {
		return
	}
	action := "delete"
	if strings.HasSuffix(c.FullPath(), "/rotate") {
		action = "rotate"
	} else if strings.HasSuffix(c.FullPath(), "/revoke") {
		action = "revoke"
	}
	secret, err := h.service.ChangeKey(c.Request.Context(), id, in.Revision, action)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"secret": secret})
}

// Intercept after the existing common audit and cache permission barrier. Every
// scoped request is terminated here: it never reaches a broad legacy DTO.
func (h *ScopedAdminHandler) Intercept(c *gin.Context) {
	value, ok := c.Get(adminaccess.PrincipalContextKey)
	if !ok {
		c.Next()
		return
	}
	c.Abort()
	principal, ok := value.(*service.ScopedAdminPrincipal)
	if !ok || h == nil {
		response.Forbidden(c, "Operation is outside this key's permissions")
		return
	}
	route := c.FullPath()
	if adminaccess.RoutePermission(c.Request.Method, route) == "" {
		response.Forbidden(c, "Operation is outside this key's permissions")
		return
	}
	kind := "accounts"
	if strings.Contains(route, "/groups") {
		kind = "groups"
	} else if strings.Contains(route, "/proxies") {
		kind = "proxies"
	}
	var id int64
	var err error
	if c.Param("id") != "" {
		id, err = strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			response.NotFound(c, "Resource not found")
			return
		}
	}
	body := []byte(`{}`)
	if c.Request.Method != "GET" && c.Request.Body != nil {
		body, err = io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10))
		if err != nil {
			response.BadRequest(c, "Request body is too large or invalid")
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			body = []byte(`{}`)
		}
	}
	page, size := response.ParsePagination(c)
	data, err := h.service.Resource(c.Request.Context(), principal, c.Request.Method, route, kind, id, page, size, body, c.GetHeader("Idempotency-Key"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, data)
}
