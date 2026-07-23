package domain

// ReasoningEffortMapping rewrites an explicit OpenAI reasoning effort before
// the group ceiling is applied.
type ReasoningEffortMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}
