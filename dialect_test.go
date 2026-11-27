package main

import (
	"encoding/json"
	"testing"
)

// OpenAI-dialect fields must be converted or stripped before hitting Mistral's
// extra=forbid validation: regression for the 422 reported in production where
// max_completion_tokens / store passed through untouched.
func TestTransformRequestOpenAIDialect(t *testing.T) {
	payload := []byte(`{
		"model": "client-name",
		"max_completion_tokens": 128000,
		"seed": 42,
		"store": false,
		"user": "some-user",
		"logit_bias": {"1": 2},
		"top_logprobs": 3,
		"stream_options": {"include_usage": true},
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	out, err := transformRequest(payload, "upstream-name", "", -1)
	if err != nil {
		t.Fatalf("transformRequest: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("output not json: %v", err)
	}
	for _, forbidden := range []string{"max_completion_tokens", "store", "user", "logit_bias", "top_logprobs", "seed"} {
		if _, exists := root[forbidden]; exists {
			t.Errorf("%s must not survive transformRequest", forbidden)
		}
	}
	var maxTokens int
	if err := json.Unmarshal(root["max_tokens"], &maxTokens); err != nil || maxTokens != 128000 {
		t.Errorf("max_completion_tokens must become max_tokens=128000, got %s", string(root["max_tokens"]))
	}
	var randomSeed int
	if err := json.Unmarshal(root["random_seed"], &randomSeed); err != nil || randomSeed != 42 {
		t.Errorf("seed must become random_seed=42, got %s", string(root["random_seed"]))
	}
	// tolerated fields pass through untouched
	if _, exists := root["stream_options"]; !exists {
		t.Error("stream_options is tolerated upstream and must pass through")
	}
}

func TestTransformRequestPrefersExistingMaxTokens(t *testing.T) {
	payload := []byte(`{"model":"x","max_tokens":100,"max_completion_tokens":128000,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := transformRequest(payload, "y", "", -1)
	if err != nil {
		t.Fatalf("transformRequest: %v", err)
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(out, &root)
	var maxTokens int
	_ = json.Unmarshal(root["max_tokens"], &maxTokens)
	if maxTokens != 100 {
		t.Errorf("explicit max_tokens must win over max_completion_tokens, got %d", maxTokens)
	}
}
