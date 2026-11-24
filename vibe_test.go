package main

import (
	"encoding/json"
	"testing"
)

func TestVibeFingerprintInjection(t *testing.T) {
	raw := []byte(`{"messages":[],"metadata":{"custom":"keep"}}`)
	got, err := applyKeyChannel(raw, apiKey{Channel: "vibe"})
	if err != nil {
		t.Fatalf("applyKeyChannel failed: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(root["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["call_source"] != "vibe_code" {
		t.Fatalf("call_source=%q", metadata["call_source"])
	}
	if metadata["custom"] != "keep" {
		t.Fatal("client metadata was clobbered")
	}
	for _, key := range []string{"host_kind", "call_type", "version", "agent_version", "agent_entrypoint", "client_name", "client_version", "session_id", "os", "arch"} {
		if metadata[key] == "" {
			t.Fatalf("missing vibe metadata key %q", key)
		}
	}
}

func TestTransformRequestDeveloperToSystem(t *testing.T) {
	raw := `{"messages":[{"role":"developer","content":"be nice"},{"role":"user","content":"hi"}]}`
	got, err := transformRequest([]byte(raw), "m1", "", -1)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if root.Messages[0].Role != "system" {
		t.Fatalf("developer role not folded: %q", root.Messages[0].Role)
	}
	if root.Messages[1].Role != "user" {
		t.Fatalf("user role changed: %q", root.Messages[1].Role)
	}
}

func TestVibeRequestHeadersChannelGated(t *testing.T) {
	if vibeRequestHeaders(apiKey{Channel: "studio"}) != nil {
		t.Fatal("studio key got vibe headers")
	}
	headers := vibeRequestHeaders(apiKey{Channel: "vibe"})
	if headers["User-Agent"] != vibeUserAgent || headers["X-Affinity"] != vibeSessionID {
		t.Fatalf("vibe headers wrong: %v", headers)
	}
}
