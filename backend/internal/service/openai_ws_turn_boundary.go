package service

// OpenAIWSLaterTurnError retains the cause for accounting but explicitly prevents
// the ingress handler from replaying its first frame after a later turn fails.
type OpenAIWSLaterTurnError struct{ Cause error }

func (e *OpenAIWSLaterTurnError) Error() string { return "later websocket turn failed" }
func (e *OpenAIWSLaterTurnError) Unwrap() error { return e.Cause }
