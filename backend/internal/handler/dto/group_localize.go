package dto

// Response copies keep localization out of shared scheduling/auth snapshots.
func (g *Group) Localized(language string) *Group {
	if g == nil {
		return nil
	}
	copy := *g
	copy.Name, copy.Description = g.I18n.Localize(language, g.Name, g.Description)
	copy.I18n = nil
	return &copy
}
func (k *APIKey) Localized(language string) *APIKey {
	if k == nil {
		return nil
	}
	copy := *k
	copy.Group = k.Group.Localized(language)
	return &copy
}
func (r *RedeemCode) Localized(language string) *RedeemCode {
	if r == nil {
		return nil
	}
	copy := *r
	copy.Group = r.Group.Localized(language)
	return &copy
}
func (s *UserSubscription) Localized(language string) *UserSubscription {
	if s == nil {
		return nil
	}
	copy := *s
	copy.Group = s.Group.Localized(language)
	return &copy
}
func (l *UsageLog) Localized(language string) *UsageLog {
	if l == nil {
		return nil
	}
	copy := *l
	copy.Group = l.Group.Localized(language)
	copy.APIKey = l.APIKey.Localized(language)
	copy.Subscription = l.Subscription.Localized(language)
	return &copy
}
