package service

import (
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func normalizeOpus55Request(body []byte, model string) ([]byte, error) {
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
