package dto

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGroupLocalizationCopiesResponse(t *testing.T) {
	group := &Group{Name: "原名", Description: "原描述", I18n: service.GroupI18n{"en": {Name: "English"}}}
	key := &APIKey{Group: group}
	localized := key.Localized("en-US,en;q=0.9")
	require.Equal(t, "English", localized.Group.Name)
	require.Equal(t, "原描述", localized.Group.Description)
	require.Nil(t, localized.Group.I18n)
	require.Equal(t, "原名", group.Name)
	require.NotNil(t, group.I18n)
	require.Equal(t, "原名", key.Localized("zh").Group.Name)
}
