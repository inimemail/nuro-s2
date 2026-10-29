// Package adminaccess describes the finite machine-admin surface. Unknown routes,
// permissions, fields and scope dimensions are denied rather than inherited.
package adminaccess

import (
	"fmt"
	"net/netip"
	"strings"
)

const PrincipalContextKey = "scoped_admin_principal"

type Rule struct {
	Permissions  []string `json:"permissions"`
	All          bool     `json:"all_resources,omitempty"`
	AccountIDs   []int64  `json:"account_ids,omitempty"`
	GroupIDs     []int64  `json:"group_ids,omitempty"`
	ProxyIDs     []int64  `json:"proxy_ids,omitempty"`
	Platforms    []string `json:"platforms,omitempty"`
	AccountTypes []string `json:"account_types,omitempty"`
	Ungrouped    bool     `json:"allow_ungrouped,omitempty"`
}
type Policy struct {
	Rules []Rule `json:"rules"`
}
type Route struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	Permission string `json:"permission"`
}

var Routes = []Route{
	{"GET", "/api/v1/admin/accounts", "accounts.read"},
	{"GET", "/api/v1/admin/accounts/:id", "accounts.read"},
	{"PUT", "/api/v1/admin/accounts/:id", "accounts.update"},
	{"POST", "/api/v1/admin/accounts", "accounts.create"},
	{"DELETE", "/api/v1/admin/accounts/:id", "accounts.delete"},
	{"GET", "/api/v1/admin/groups", "groups.read"},
	{"GET", "/api/v1/admin/groups/:id", "groups.read"},
	{"PUT", "/api/v1/admin/groups/:id", "groups.update"},
	{"GET", "/api/v1/admin/proxies", "proxies.read"},
}
var Permissions = []string{"accounts.read", "accounts.update", "accounts.create", "accounts.delete", "accounts.models.read", "accounts.models.write", "accounts.notes.write", "accounts.status.write", "accounts.network.write", "accounts.credentials.write", "accounts.schedule.write", "accounts.groups.write", "groups.read", "groups.update", "proxies.read"}
var platforms = map[string]bool{"openai": true, "anthropic": true, "gemini": true, "antigravity": true, "grok": true, "deepseek": true, "kimi": true, "zhipu": true, "minimax": true, "opencode_go": true, "typesafe": true, "composite": true}
var types = map[string]bool{"apikey": true, "oauth": true, "setup-token": true, "upstream": true, "service_account": true, "bedrock": true}

func Contains[T comparable](items []T, value T) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
func (p Policy) Validate() error {
	if len(p.Rules) == 0 || len(p.Rules) > 32 {
		return fmt.Errorf("policy requires 1–32 rules")
	}
	for _, r := range p.Rules {
		if len(r.Permissions) == 0 || len(r.Permissions) > len(Permissions) {
			return fmt.Errorf("invalid permissions")
		}
		for _, permission := range r.Permissions {
			if !Contains(Permissions, permission) {
				return fmt.Errorf("unknown permission: %s", permission)
			}
		}
		if !r.All && len(r.AccountIDs)+len(r.GroupIDs)+len(r.ProxyIDs) == 0 && !r.Ungrouped {
			return fmt.Errorf("explicit resource scope required")
		}
		for _, ids := range [][]int64{r.AccountIDs, r.GroupIDs, r.ProxyIDs} {
			if len(ids) > 200 {
				return fmt.Errorf("resource scope exceeds 200 IDs")
			}
			for _, id := range ids {
				if id <= 0 {
					return fmt.Errorf("resource IDs must be positive")
				}
			}
		}
		for _, platform := range r.Platforms {
			if !platforms[platform] {
				return fmt.Errorf("unsupported scoped platform")
			}
		}
		for _, typ := range r.AccountTypes {
			if !types[typ] {
				return fmt.Errorf("unsupported scoped account type")
			}
		}
	}
	return nil
}
func RoutePermission(method, path string) string {
	for _, r := range Routes {
		if r.Method == method && r.Path == path {
			return r.Permission
		}
	}
	return ""
}
func (p Policy) Has(permission string) bool {
	for _, r := range p.Rules {
		if Contains(r.Permissions, permission) {
			return true
		}
	}
	return false
}
func (r Rule) AccountMatches(permission string, id int64, platform, typ string, groups []int64) bool {
	if !Contains(r.Permissions, permission) || len(r.Platforms) > 0 && !Contains(r.Platforms, platform) || len(r.AccountTypes) > 0 && !Contains(r.AccountTypes, typ) {
		return false
	}
	if r.All || Contains(r.AccountIDs, id) {
		return true
	}
	if len(groups) == 0 && r.Ungrouped {
		return true
	}
	for _, id := range groups {
		if Contains(r.GroupIDs, id) {
			return true
		}
	}
	return false
}
func (p Policy) AccountMatches(permission string, id int64, platform, typ string, groups []int64) bool {
	for _, r := range p.Rules {
		if r.AccountMatches(permission, id, platform, typ, groups) {
			return true
		}
	}
	return false
}
func (p Policy) GroupMatches(permission string, id int64, platform string) bool {
	for _, r := range p.Rules {
		if Contains(r.Permissions, permission) && (r.All || Contains(r.GroupIDs, id)) &&
			(len(r.Platforms) == 0 || Contains(r.Platforms, platform)) {
			return true
		}
	}
	return false
}
func (p Policy) ProxyMatches(permission string, id int64) bool {
	for _, r := range p.Rules {
		if Contains(r.Permissions, permission) && (r.All || Contains(r.ProxyIDs, id)) {
			return true
		}
	}
	return false
}
func ValidateIPPolicy(values []string) error {
	if len(values) > 32 {
		return fmt.Errorf("at most 32 IP rules")
	}
	for _, v := range values {
		if _, err := netip.ParsePrefix(v); err != nil {
			if _, err = netip.ParseAddr(v); err != nil {
				return fmt.Errorf("invalid IP or CIDR")
			}
		}
	}
	return nil
}
func IPAllowed(values []string, client string) bool {
	if len(values) == 0 {
		return true
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(client))
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, v := range values {
		if network, err := netip.ParsePrefix(v); err == nil && network.Contains(ip) {
			return true
		}
		if exact, err := netip.ParseAddr(v); err == nil && exact.Unmap() == ip {
			return true
		}
	}
	return false
}

func PlatformSupported(value string) bool { return platforms[value] }
