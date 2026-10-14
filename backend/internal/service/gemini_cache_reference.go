package service

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"strings"
)

type geminiCacheBoundContextKey struct{}
type geminiCacheCandidateContextKey struct{}

func withGeminiCacheCandidates(ctx context.Context) context.Context {
	return context.WithValue(ctx, geminiCacheCandidateContextKey{}, true)
}

func acceptsGeminiCacheCandidate(ctx context.Context, account *Account) bool {
	required, _ := ctx.Value(geminiCacheCandidateContextKey{}).(bool)
	return !required || IsExplicitCacheAccount(account)
}

func WithGeminiCacheBoundRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, geminiCacheBoundContextKey{}, true)
}
func isGeminiCacheBoundRequest(ctx context.Context) bool {
	ok, _ := ctx.Value(geminiCacheBoundContextKey{}).(bool)
	return ok
}

// This lookup reads only the native reference field. Plain requests retain
// their existing bytes; local references make one targeted field replacement.
func FindGeminiCacheReference(body []byte, action string) (id, path string, local bool, error error) {
	path = "cachedContent"
	if action == "countTokens" && gjson.GetBytes(body, "generateContentRequest").Exists() {
		path = "generateContentRequest.cachedContent"
	}
	field := gjson.GetBytes(body, path)
	if !field.Exists() {
		return "", path, false, nil
	}
	if field.Type != gjson.String {
		return "", path, false, nil
	}
	name := field.Str
	if !strings.HasPrefix(name, "cachedContents/nuro_") {
		return "", path, false, nil
	}
	id = strings.TrimPrefix(name, "cachedContents/")
	if !ValidGeminiCachePublicID(id) || !json.Valid(body) || !uniqueJSONValue(json.NewDecoder(bytes.NewReader(body)), 0) {
		return "", path, true, ErrGeminiCacheNotFound
	}
	return id, path, true, nil
}
func RewriteGeminiCacheReference(body []byte, path, name string) ([]byte, error) {
	return sjson.SetBytes(body, path, name)
}

// Vertex caches are regional resources. Changing an account's model-region
// configuration must not redirect an existing cache reference to a new region.
// This only copies credentials for bound native requests; plain requests keep
// their original account and existing routing behavior.
func GeminiCacheBoundAccount(account *Account, name string) (*Account, error) {
	if account == nil {
		return nil, ErrGeminiCacheUnavailable
	}
	if !account.IsVertexServiceAccount() {
		return account, nil
	}
	if !geminiCacheUpstreamNamePattern.MatchString(name) {
		return nil, ErrGeminiCacheConflict
	}
	parts := strings.Split(name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[1] != account.VertexProjectID() || parts[2] != "locations" || !vertexLocationPattern.MatchString(parts[3]) {
		return nil, ErrGeminiCacheConflict
	}
	copy := *account
	copy.Credentials = make(map[string]any, len(account.Credentials))
	for key, value := range account.Credentials {
		copy.Credentials[key] = value
	}
	copy.Credentials["location"] = parts[3]
	delete(copy.Credentials, "vertex_model_locations")
	return &copy, nil
}
