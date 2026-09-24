package service

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeGPT6ResponsesSampling operates only on the final Sol/Luna request.
// mode and effort are independent; effort=none permits sampling.
func normalizeGPT6ResponsesSampling(body []byte) ([]byte, error) {
	if !openai.IsGPT6SolOrLunaModelSpelling(gjson.GetBytes(body, "model").String()) || gjson.GetBytes(body, "reasoning.effort").String() == "none" {
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
