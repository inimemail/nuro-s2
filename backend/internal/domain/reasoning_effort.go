package domain

const (
	ReasoningEffortMatchExact  = "exact"
	ReasoningEffortMatchPrefix = "prefix"
	ReasoningEffortMatchSuffix = "suffix"
)

// ReasoningEffortMapping rewrites an explicit OpenAI reasoning effort before
// the group ceiling is applied. Empty model and match type preserve the
// legacy model-agnostic behavior.
type ReasoningEffortMapping struct {
	From      string `json:"from"`
	To        string `json:"to"`
	MatchType string `json:"match_type,omitempty"`
	Model     string `json:"model,omitempty"`
}
