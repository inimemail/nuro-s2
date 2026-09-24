package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"sort"
)

// Resolved names are admin-only picker metadata; requests retain the public ID.
type openAIAccountTestModel struct {
	openai.Model
	UpstreamModel string `json:"upstream_model,omitempty"`
}
type claudeAccountTestModel struct {
	claude.Model
	UpstreamModel string `json:"upstream_model,omitempty"`
}
type geminiAccountTestModel struct {
	geminicli.Model
	UpstreamModel string `json:"upstream_model,omitempty"`
}

func mappedOpenAITestModels(models []openai.Model, mapping map[string]string) []openAIAccountTestModel {
	out := make([]openAIAccountTestModel, 0, len(models))
	for _, model := range models {
		out = append(out, openAIAccountTestModel{model, mapping[model.ID]})
	}
	if len(mapping) > 0 {
		sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	}
	return out
}
func mappedClaudeTestModels(models []claude.Model, mapping map[string]string) []claudeAccountTestModel {
	out := make([]claudeAccountTestModel, 0, len(models))
	for _, model := range models {
		out = append(out, claudeAccountTestModel{model, mapping[model.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func mappedGeminiTestModels(models []geminicli.Model, mapping map[string]string) []geminiAccountTestModel {
	out := make([]geminiAccountTestModel, 0, len(models))
	for _, model := range models {
		out = append(out, geminiAccountTestModel{model, mapping[model.ID]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
