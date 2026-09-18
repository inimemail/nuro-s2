package admin

import (
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The upstream model lets the picker classify custom aliases without changing
// the model ID that the administrator actually tests.
type grokAccountTestModel struct {
	xai.Model
	UpstreamModel string `json:"upstream_model,omitempty"`
}

func grokAccountTestModels(account *service.Account) []grokAccountTestModel {
	defaults := xai.DefaultModels()
	hasExplicitMapping := false
	switch mapping := account.Credentials["model_mapping"].(type) {
	case map[string]any:
		hasExplicitMapping = len(mapping) > 0
	case map[string]string:
		hasExplicitMapping = len(mapping) > 0
	}
	models := make([]grokAccountTestModel, 0, len(defaults))
	if !hasExplicitMapping {
		for _, model := range defaults {
			models = append(models, grokAccountTestModel{Model: model})
		}
		return models
	}
	byID := make(map[string]xai.Model, len(defaults))
	for _, model := range defaults {
		byID[model.ID] = model
	}
	for requested, upstream := range account.GetModelMapping() {
		model, ok := byID[requested]
		if !ok {
			model = xai.Model{ID: requested, Object: "model", Type: "model", OwnedBy: "xai", DisplayName: requested}
		}
		models = append(models, grokAccountTestModel{Model: model, UpstreamModel: upstream})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}
