package service

import "strings"

// resolveOpenAIForwardModel 解析 OpenAI 兼容转发使用的模型。
// messagesDispatchMappedModel 是调用方已为 /v1/messages 解析的显式调度结果；
// 普通 OpenAI 请求必须传空，避免将分组配置作为通用模型兜底。
func resolveOpenAIForwardModel(account *Account, requestedModel, messagesDispatchMappedModel string) string {
	messagesDispatchMappedModel = strings.TrimSpace(messagesDispatchMappedModel)
	// The current custom call graph still carries this argument through a few
	// shared helpers. Only Claude Messages requests may consume it; ordinary
	// OpenAI model names must never fall back to a group Messages default.
	useMessagesDispatchModel := messagesDispatchMappedModel != "" &&
		strings.HasPrefix(strings.ToLower(strings.TrimSpace(requestedModel)), "claude")
	if account == nil {
		if useMessagesDispatchModel {
			return messagesDispatchMappedModel
		}
		return requestedModel
	}

	mappedModel, matched := account.ResolveMappedModel(requestedModel)
	if !matched && useMessagesDispatchModel {
		return messagesDispatchMappedModel
	}
	return mappedModel
}

var openAIOAuthForeignModelPrefixes = []string{
	"deepseek-",
	"glm-",
	"kimi-",
	"moonshot-",
	"qwen-",
	"qwen2-",
	"qwen3-",
	"qwen4-",
	"qwq-",
	"minimax-",
	"gemini-",
	"gemma-",
	"grok-",
	"doubao-",
	"hunyuan-",
	"llama-",
	"llama2-",
	"llama3-",
	"meta-llama",
	"mistral-",
	"mixtral-",
	"baichuan-",
	"ernie-",
	"step-",
	"seed-",
	"yi-",
}

func isOpenAIOAuthServableModel(requestedModel string) bool {
	model := strings.ToLower(lastOpenAIModelSegment(requestedModel))
	if model == "" {
		return true
	}
	for _, prefix := range openAIOAuthForeignModelPrefixes {
		if strings.HasPrefix(model, prefix) {
			return false
		}
	}
	return true
}

// resolveOpenAICompactForwardModel determines the compact-only upstream model
// for /responses/compact requests. It never affects normal /responses traffic.
// When no compact-specific mapping matches, the input model is returned as-is.
func resolveOpenAICompactForwardModel(account *Account, model string) string {
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" || account == nil {
		return trimmedModel
	}

	mappedModel, matched := account.ResolveCompactMappedModel(trimmedModel)
	if !matched {
		return trimmedModel
	}
	if trimmedMapped := strings.TrimSpace(mappedModel); trimmedMapped != "" {
		return trimmedMapped
	}
	return trimmedModel
}

func resolveOpenAICompactForwardModelWithFallback(account *Account, model, fallback string) string {
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return trimmedModel
	}
	if account != nil {
		mappedModel, matched := account.ResolveCompactMappedModel(trimmedModel)
		if matched {
			if trimmedMapped := strings.TrimSpace(mappedModel); trimmedMapped != "" {
				return trimmedMapped
			}
			return trimmedModel
		}
	}
	if trimmedFallback := strings.TrimSpace(fallback); trimmedFallback != "" {
		return trimmedFallback
	}
	return trimmedModel
}
