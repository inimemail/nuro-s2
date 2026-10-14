package service

import (
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeOpus55Request(body []byte, model string) ([]byte, error) {
	if claude.IsHaiku55(model) {
		return normalizeHaiku55Request(body, model)
	}
	if !claude.IsAdaptiveThinkingModel(model) {
		return body, nil
	}
	if thinking := gjson.GetBytes(body, "thinking.type").String(); thinking != "" && thinking != "adaptive" {
		return nil, fmt.Errorf("%s requires adaptive thinking", model)
	}
	if gjson.GetBytes(body, "thinking.budget_tokens").Exists() {
		return nil, fmt.Errorf("%s does not support thinking.budget_tokens", model)
	}
	if choice := gjson.GetBytes(body, "tool_choice.type").String(); choice == "any" || choice == "tool" {
		return nil, fmt.Errorf("%s requires tool_choice auto or none", model)
	}
	switch effort := gjson.GetBytes(body, "output_config.effort").String(); effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return nil, fmt.Errorf("unsupported %s effort %q", model, effort)
	}
	out := body
	var err error
	if !gjson.GetBytes(out, "thinking.type").Exists() {
		out, err = sjson.SetBytes(out, "thinking.type", "adaptive")
		if err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"temperature", "top_p", "top_k"} {
		if gjson.GetBytes(out, key).Exists() {
			out, err = sjson.DeleteBytes(out, key)
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func normalizeHaiku55Request(body []byte, model string) ([]byte, error) {
	thinking := gjson.GetBytes(body, "thinking.type").String()
	if thinking != "" && thinking != "adaptive" && thinking != "disabled" {
		return nil, fmt.Errorf("%s supports adaptive or disabled thinking", model)
	}
	effort := gjson.GetBytes(body, "output_config.effort").String()
	switch effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return nil, fmt.Errorf("unsupported %s effort %q", model, effort)
	}
	if thinking == "disabled" && (effort == "xhigh" || effort == "max") {
		return nil, fmt.Errorf("%s disabled thinking supports low, medium or high effort", model)
	}
	if gjson.GetBytes(body, "thinking.budget_tokens").Exists() || gjson.GetBytes(body, "top_k").Exists() {
		return nil, fmt.Errorf("%s does not support thinking.budget_tokens or top_k", model)
	}
	temperature, topP := gjson.GetBytes(body, "temperature"), gjson.GetBytes(body, "top_p")
	if temperature.Exists() && (temperature.Type != gjson.Number || temperature.Float() != 1) {
		return nil, fmt.Errorf("%s supports only temperature=1", model)
	}
	if topP.Exists() && (topP.Type != gjson.Number || topP.Float() != 0.99) {
		return nil, fmt.Errorf("%s supports only top_p=0.99", model)
	}
	if temperature.Exists() && topP.Exists() {
		return nil, fmt.Errorf("%s does not allow temperature and top_p together", model)
	}
	if thinking == "" {
		return sjson.SetBytes(body, "thinking.type", "adaptive")
	}
	return body, nil
}
