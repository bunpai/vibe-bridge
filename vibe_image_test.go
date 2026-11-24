package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func imgPart(n int) string {
	return fmt.Sprintf(`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA%d"}}`, n)
}

func TestTransformRequestImageTrim(t *testing.T) {
	raw := `{"messages":[
		{"role":"user","content":[` + imgPart(1) + `]},
		{"role":"user","content":[{"type":"text","text":"q"},` + imgPart(2) + `,` + imgPart(3) + `]}]}`
	got, err := transformRequest([]byte(raw), "m1", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if root.Messages[0].Content[0].Type != "text" || !strings.Contains(root.Messages[0].Content[0].Text, "only the 2 most recent") {
		t.Fatalf("oldest image not replaced: %+v", root.Messages[0].Content[0])
	}
	for i, p := range root.Messages[1].Content[1:] {
		if p.Type != "image_url" {
			t.Fatalf("recent image %d was trimmed", i)
		}
	}
}

func TestTransformRequestImageTrimDisabled(t *testing.T) {
	raw := `{"messages":[{"role":"user","content":[` + imgPart(1) + `,` + imgPart(2) + `]}]}`
	got, err := transformRequest([]byte(raw), "m1", "", -1)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	if root.Messages[0].Content[0].Type != "image_url" {
		t.Fatal("image trimmed despite negative limit")
	}
}

func TestImageLimitFor(t *testing.T) {
	if n := imageLimitFor(pluginState{}, modelMapping{}); n != officialMaxRequestImages {
		t.Fatalf("default = %d, want %d", n, officialMaxRequestImages)
	}
	if n := imageLimitFor(pluginState{ImageLimit: 12}, modelMapping{}); n != 12 {
		t.Fatalf("state default ignored: %d", n)
	}
	if n := imageLimitFor(pluginState{ImageLimit: 12}, modelMapping{ImageLimit: 20}); n != 20 {
		t.Fatalf("model override ignored: %d", n)
	}
}
