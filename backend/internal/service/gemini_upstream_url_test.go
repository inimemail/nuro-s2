package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildGeminiAIStudioModelActionURL(t *testing.T) {
	got, err := buildGeminiAIStudioModelActionURL("https://generativelanguage.googleapis.com/", "gemini-2.5-pro", "streamGenerateContent", true)
	require.NoError(t, err)
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse", got)
	_, err = buildGeminiAIStudioModelActionURL("https://example.com", "../../x", "generateContent", false)
	require.Error(t, err)
}
