package openai

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// GPT62ModelID recognizes only published models and known local effort/compact
// aliases. Provider-defined names and future suffixes keep their old behavior.
func GPT62ModelID(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	id = strings.ReplaceAll(id, "_", "-")
	for _, base := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		if id == base {
			return base
		}
		if suffix, ok := strings.CutPrefix(id, base+"-"); ok {
			switch suffix {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max", "openai-compact":
				return base
			}
		}
	}
	return ""
}

func GPT62AliasEffort(model string) string {
	base := GPT62ModelID(model)
	if base == "" {
		return ""
	}
	id := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), "_", "-")
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	suffix, _ := strings.CutPrefix(id, base+"-")
	if suffix == "openai-compact" || suffix == id {
		return ""
	}
	return suffix
}

func ValidateGPT62ModelRequest(model, effort string) error {
	base := GPT62ModelID(model)
	if base == "" {
		return nil
	}
	for _, value := range []string{GPT62AliasEffort(model), strings.ToLower(strings.TrimSpace(effort))} {
		switch value {
		case "none", "minimal", "ultra":
			return fmt.Errorf("%s does not support API reasoning effort %q; use low, medium, high, xhigh or max", base, value)
		}
	}
	return nil
}

// Model descriptors are immutable resources. Decode instructions once at
// initialization, never on the request/first-token path.
//
//go:embed codex_gpt62_sol.json
var codexGPT62SolMetadata []byte

//go:embed codex_gpt62_astra.json
var codexGPT62AstraMetadata []byte

func gpt62Instructions(raw []byte) string {
	var descriptor struct {
		BaseInstructions string `json:"base_instructions"`
		ModelMessages    struct {
			InstructionsTemplate string `json:"instructions_template"`
		} `json:"model_messages"`
	}
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		panic("invalid embedded GPT-6.2 descriptor: " + err.Error())
	}
	if value := descriptor.ModelMessages.InstructionsTemplate; strings.TrimSpace(value) != "" {
		return value
	}
	if strings.TrimSpace(descriptor.BaseInstructions) != "" {
		return descriptor.BaseInstructions
	}
	panic("missing embedded GPT-6.2 instructions")
}

var instructionsGPT62Sol = gpt62Instructions(codexGPT62SolMetadata)
var instructionsGPT62Astra = gpt62Instructions(codexGPT62AstraMetadata)
