package service

import (
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"regexp"
	"strings"
	"unicode/utf8"
)

type GroupI18n = domain.GroupI18n
type GroupLocaleText = domain.GroupLocaleText

var groupLocalePattern = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)

func normalizeGroupI18n(input GroupI18n) (GroupI18n, error) {
	if len(input) > 16 {
		return nil, fmt.Errorf("group i18n supports at most 16 locales")
	}
	result := make(GroupI18n, len(input))
	seen := make(map[string]bool, len(input))
	for raw, text := range input {
		if len(raw) > 35 || !groupLocalePattern.MatchString(raw) {
			return nil, fmt.Errorf("invalid group locale")
		}
		locale := domain.CanonicalLocale(raw)
		if locale == "und" || seen[locale] {
			return nil, fmt.Errorf("invalid or duplicate group locale")
		}
		seen[locale] = true
		text.Name, text.Description = strings.TrimSpace(text.Name), strings.TrimSpace(text.Description)
		if !utf8.ValidString(text.Name) || !utf8.ValidString(text.Description) || utf8.RuneCountInString(text.Name) > 100 || utf8.RuneCountInString(text.Description) > 2000 {
			return nil, fmt.Errorf("group translation exceeds its text limit")
		}
		if text.Name != "" || text.Description != "" {
			result[locale] = text
		}
	}
	return result, nil
}
