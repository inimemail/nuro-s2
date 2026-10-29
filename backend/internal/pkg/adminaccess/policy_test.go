package adminaccess

import "testing"

func TestPolicyKeepsRulesAndRoutesSeparate(t *testing.T) {
	p := Policy{Rules: []Rule{
		{Permissions: []string{"accounts.update"}, AccountIDs: []int64{1}, Platforms: []string{"openai"}},
		{Permissions: []string{"accounts.read"}, AccountIDs: []int64{2}, Platforms: []string{"grok"}},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if !p.AccountMatches("accounts.update", 1, "openai", "apikey", nil) || p.AccountMatches("accounts.update", 1, "grok", "apikey", nil) || p.AccountMatches("accounts.update", 2, "openai", "apikey", nil) {
		t.Fatal("scope dimensions were combined")
	}
	for _, path := range []string{"/api/v1/admin/accounts/data", "/api/v1/admin/accounts/:id/export", "/api/v1/admin/settings/admin-access-keys", "/api/v1/admin/accounts/:id/reset-quota"} {
		if RoutePermission("POST", path) != "" {
			t.Fatalf("unexpected route: %s", path)
		}
	}
	if RoutePermission("PATCH", "/api/v1/admin/accounts/:id") != "" {
		t.Fatal("method inferred")
	}
	p.Rules[0].Permissions = []string{"accounts.*"}
	if p.Validate() == nil {
		t.Fatal("wildcard permission accepted")
	}
}

func TestIPPolicies(t *testing.T) {
	rules := []string{"192.0.2.0/24", "2001:db8::/32"}
	for _, ip := range []string{"192.0.2.7", "::ffff:192.0.2.7", "2001:db8::1"} {
		if !IPAllowed(rules, ip) {
			t.Fatal(ip)
		}
	}
	for _, ip := range []string{"198.51.100.1", "unknown", "192.0.2.1, 198.51.100.1"} {
		if IPAllowed(rules, ip) {
			t.Fatal(ip)
		}
	}
	if ValidateIPPolicy([]string{"192.0.2.0/33"}) == nil {
		t.Fatal("invalid CIDR")
	}
}

func TestGroupPolicyKeepsPlatformInTheSameRule(t *testing.T) {
	p := Policy{Rules: []Rule{
		{Permissions: []string{"groups.update"}, GroupIDs: []int64{1}, Platforms: []string{"openai"}},
		{Permissions: []string{"groups.read"}, All: true, Platforms: []string{"grok"}},
	}}
	if !p.GroupMatches("groups.update", 1, "openai") || p.GroupMatches("groups.update", 1, "grok") ||
		p.GroupMatches("groups.update", 2, "openai") || p.GroupMatches("groups.read", 1, "openai") ||
		!p.GroupMatches("groups.read", 2, "grok") {
		t.Fatal("group scope dimensions were combined or ignored")
	}
}
