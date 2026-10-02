package typesafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrStreamingUnsupported = errors.New("typesafe system one does not support streaming")

const maxSystemOneJSONNestingDepth = 128

func ValidateSystemOneRequest(body []byte) (string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return "", errors.New("invalid JSON request")
	}
	if err := rejectDuplicateFields(body, "request"); err != nil {
		return "", err
	}
	if err := checkKeys(body, "request", []string{"model", "state", "questions", "stream"}); err != nil {
		return "", err
	}
	model, err := requiredString(envelope["model"], "model")
	if err != nil || model != JevLatestModel {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("model must be %s", JevLatestModel)
	}
	if err := validateStringObjectOrArray(envelope["state"], "state"); err != nil {
		return "", err
	}
	questionsRaw := bytes.TrimSpace(envelope["questions"])
	var questions map[string]json.RawMessage
	if len(questionsRaw) == 0 || questionsRaw[0] != '{' || json.Unmarshal(questionsRaw, &questions) != nil || len(questions) == 0 {
		return "", errors.New("questions must be a non-empty object")
	}
	if err := checkKeys(questionsRaw, "questions", nil); err != nil {
		return "", err
	}
	if raw := bytes.TrimSpace(envelope["stream"]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var stream bool
		if json.Unmarshal(raw, &stream) != nil {
			return "", errors.New("stream must be a boolean")
		}
		if stream {
			return "", ErrStreamingUnsupported
		}
	}
	for id, raw := range questions {
		if err := validateQuestion(id, raw); err != nil {
			return "", err
		}
	}
	return model, nil
}

// rejectDuplicateFields walks every JSON object instead of relying on
// json.Unmarshal into a map, which otherwise silently keeps the last value for
// duplicate keys. Nested criteria are part of the protocol and must be
// deterministic for both forwarding and billing.
func rejectDuplicateFields(raw []byte, scope string) error {
	return rejectDuplicateFieldsAtDepth(raw, scope, 0)
}

func rejectDuplicateFieldsAtDepth(raw []byte, scope string, depth int) error {
	if depth > maxSystemOneJSONNestingDepth {
		return errors.New("request JSON nesting is too deep")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func(*json.Decoder, string) error
	walk = func(decoder *json.Decoder, currentScope string) error {
		tok, err := decoder.Token()
		if err != nil {
			return errors.New("invalid JSON request")
		}
		switch delimiter := tok.(type) {
		case json.Delim:
			switch delimiter {
			case '{':
				seen := map[string]struct{}{}
				for decoder.More() {
					keyToken, err := decoder.Token()
					if err != nil {
						return errors.New("invalid JSON request")
					}
					key, ok := keyToken.(string)
					if !ok {
						return errors.New("invalid JSON request")
					}
					if _, exists := seen[key]; exists {
						return fmt.Errorf("%s contains duplicate field %q", currentScope, key)
					}
					seen[key] = struct{}{}
					var value json.RawMessage
					if err := decoder.Decode(&value); err != nil {
						return errors.New("invalid JSON request")
					}
					if err := rejectDuplicateFieldsAtDepth(value, currentScope+"."+key, depth+1); err != nil {
						return err
					}
				}
				if _, err := decoder.Token(); err != nil {
					return errors.New("invalid JSON request")
				}
			case '[':
				for index := 0; decoder.More(); index++ {
					var value json.RawMessage
					if err := decoder.Decode(&value); err != nil {
						return errors.New("invalid JSON request")
					}
					if err := rejectDuplicateFieldsAtDepth(value, fmt.Sprintf("%s[%d]", currentScope, index), depth+1); err != nil {
						return err
					}
				}
				if _, err := decoder.Token(); err != nil {
					return errors.New("invalid JSON request")
				}
			}
		}
		return nil
	}
	return walk(decoder, scope)
}

func validateQuestion(id string, raw json.RawMessage) error {
	raw = bytes.TrimSpace(raw)
	var q map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &q) != nil || q == nil {
		return fmt.Errorf("question %q must be an object", id)
	}
	if err := checkKeys(raw, fmt.Sprintf("question %q", id), []string{"type", "instructions", "criteria"}); err != nil {
		return err
	}
	typ, err := requiredString(q["type"], "question type")
	if err != nil {
		return fmt.Errorf("question %q: %w", id, err)
	}
	if err := validateOptionalDescription(q["instructions"], "instructions"); err != nil {
		return fmt.Errorf("question %q: %w", id, err)
	}
	switch typ {
	case "noul":
		if raw := bytes.TrimSpace(q["criteria"]); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			var criteria map[string]json.RawMessage
			if raw[0] != '{' || json.Unmarshal(raw, &criteria) != nil {
				return fmt.Errorf("question %q: noul criteria must be an object", id)
			}
			for _, k := range []string{"true", "false"} {
				if err := validateOptionalDescription(criteria[k], "noul criteria "+k); err != nil {
					return fmt.Errorf("question %q: %w", id, err)
				}
			}
		}
	case "choice":
		var criteria map[string]json.RawMessage
		if json.Unmarshal(q["criteria"], &criteria) != nil || criteria == nil {
			return fmt.Errorf("question %q: choice criteria must be an object", id)
		}
		for _, v := range criteria {
			if err := validateOptionalDescription(v, "choice criteria value"); err != nil {
				return fmt.Errorf("question %q: %w", id, err)
			}
		}
	case "score":
		var criteria []json.RawMessage
		if json.Unmarshal(q["criteria"], &criteria) != nil || len(criteria) == 0 {
			return fmt.Errorf("question %q: score criteria must contain at least one level", id)
		}
		for _, v := range criteria {
			if err := validateStringObjectOrArray(v, "score criteria value"); err != nil {
				return fmt.Errorf("question %q: %w", id, err)
			}
		}
	default:
		return fmt.Errorf("question %q: unsupported type %q", id, typ)
	}
	return nil
}

func requiredString(raw json.RawMessage, name string) (string, error) {
	var v string
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", name)
	}
	return v, nil
}

func validateOptionalDescription(raw json.RawMessage, name string) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	return validateStringObjectOrArray(raw, name)
}

func validateStringObjectOrArray(raw json.RawMessage, name string) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("%s is required", name)
	}
	if raw[0] == '{' || raw[0] == '[' {
		return nil
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return nil
	}
	return fmt.Errorf("%s must be a string, object, or array", name)
}

func checkKeys(raw []byte, scope string, canonical []string) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	tok, err := decoder.Token()
	if err != nil || tok != json.Delim('{') {
		return fmt.Errorf("%s must be an object", scope)
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		t, err := decoder.Token()
		if err != nil {
			return errors.New("invalid JSON request")
		}
		key := t.(string)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%s contains duplicate field %q", scope, key)
		}
		seen[key] = struct{}{}
		for _, field := range canonical {
			if key != field && strings.EqualFold(key, field) {
				return fmt.Errorf("%s field %q must be written as %q", scope, key, field)
			}
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return errors.New("invalid JSON request")
		}
	}
	return nil
}
