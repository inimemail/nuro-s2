package service

import (
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeGPT6ResponsesSampling operates only on the final Sol/Luna request.
// mode and effort are independent; effort=none permits sampling.
func normalizeGPT6ResponsesSampling(body []byte) ([]byte, error) {
	model := gjson.GetBytes(body, "model").String()
	effort := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if effort == "" {
		effort = strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String())
	}
	if effort == "" {
		effort = strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String())
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()), "disabled") {
		effort = "none"
	}
	if openai.IsGPT61SolModelSpelling(model) {
		if err := openai.ValidateGPT61SolModelRequest(model, effort); err != nil {
			return nil, err
		}
	}
	if !openai.IsGPT6SolOrLunaModelSpelling(model) && !openai.IsGPT61SolModelSpelling(model) && !openai.IsGPT61TerraModelSpelling(model) || ((openai.IsGPT6SolOrLunaModelSpelling(model) || openai.IsGPT61TerraModelSpelling(model)) && strings.EqualFold(effort, "none")) {
		return body, nil
	}
	out := body
	for _, key := range []string{"temperature", "top_p", "top_logprobs", "logprobs"} {
		if !gjson.GetBytes(out, key).Exists() {
			continue
		}
		var err error
		out, err = sjson.DeleteBytes(out, key)
		if err != nil {
			return nil, fmt.Errorf("normalize GPT-6 sampling: %w", err)
		}
	}
	if includes := gjson.GetBytes(out, "include"); includes.IsArray() {
		items := includes.Array()
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].String() != "message.output_text.logprobs" {
				continue
			}
			var err error
			out, err = sjson.DeleteBytes(out, fmt.Sprintf("include.%d", i))
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
