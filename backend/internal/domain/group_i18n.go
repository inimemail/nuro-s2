package domain

import (
	"golang.org/x/text/language"
	"strings"
)

type GroupLocaleText struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}
type GroupI18n map[string]GroupLocaleText

func CanonicalLocale(locale string) string {
	return language.Make(strings.ReplaceAll(locale, "_", "-")).String()
}

func (m GroupI18n) Localize(acceptLanguage, name, description string) (string, string) {
	tags, _, err := language.ParseAcceptLanguage(acceptLanguage)
	if err != nil || len(tags) == 0 {
		return name, description
	}
	base, _ := tags[0].Base()
	for _, candidate := range []string{tags[0].String(), base.String()} {
		text, found := m[candidate]
		if !found {
			continue
		}
		if text.Name != "" {
			name = text.Name
		}
		if text.Description != "" {
			description = text.Description
		}
		return name, description
	}
	return name, description
}

func (m GroupI18n) LocalizeName(language, name string) string {
	name, _ = m.Localize(language, name, "")
	return name
}

func (m GroupI18n) Clone() GroupI18n {
	copy := make(GroupI18n, len(m))
	for locale, text := range m {
		copy[locale] = text
	}
	return copy
}
