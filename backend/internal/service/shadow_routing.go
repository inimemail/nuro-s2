package service

// parentHealthyForShadow reports whether a spark shadow account can use its
// parent credentials. Non-shadow accounts are always allowed by this gate.
func parentHealthyForShadow(account *Account, lookup func(int64) *Account) bool {
	if account == nil || !account.IsShadow() {
		return true
	}
	if lookup == nil || account.ParentAccountID == nil {
		return false
	}
	parent := lookup(*account.ParentAccountID)
	if parent == nil {
		return false
	}
	return parent.IsOpenAIOAuth() && parent.IsCredentialUsableForShadow()
}

func sparkModelVariants() []string {
	out := make([]string, 0, 1)
	for alias, target := range codexModelMap {
		if target == "gpt-5.3-codex-spark" {
			out = append(out, alias)
		}
	}
	return out
}

func defaultSparkShadowModelMapping() map[string]any {
	variants := sparkModelVariants()
	mapping := make(map[string]any, len(variants))
	for _, m := range variants {
		mapping[m] = m
	}
	return mapping
}
