package main

import "testing"

func TestRequestMayContainMedia(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantMedia bool
		wantValid bool
	}{
		{name: "plain chat", raw: `{"messages":[{"role":"user","content":"image_url and PDF are ordinary text here"}]}`, wantValid: true},
		{name: "tool definition", raw: `{"messages":[{"role":"user","content":"run"}],"tools":[{"type":"function","function":{"name":"exec"}}]}`, wantValid: true},
		{name: "openai image", raw: `{"messages":[{"role":"user","content":[{"type" : "image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}]}`, wantMedia: true, wantValid: true},
		{name: "responses image", raw: `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/a.png"}]}]}`, wantMedia: true, wantValid: true},
		{name: "claude image", raw: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}}]}]}`, wantMedia: true, wantValid: true},
		{name: "escaped key", raw: `{"messages":[{"role":"user","content":[{"ty\u0070e":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}]}`, wantMedia: true, wantValid: true},
		{name: "unsupported PDF", raw: `{"messages":[{"role":"user","content":[{"type":"document","source":{"media_type":"application/pdf"}}]}]}`, wantMedia: true, wantValid: true},
		{name: "invalid plain JSON", raw: `{"messages":[`, wantValid: false},
		{name: "invalid media JSON defers validation", raw: `{"type":"image_url"`, wantMedia: true, wantValid: true},
		{name: "invalid number", raw: `{"messages":[{"role":"user","content":01}]}`, wantValid: false},
		{name: "invalid escape", raw: `{"messages":[{"role":"user","content":"bad\q"}]}`, wantValid: false},
		{name: "trailing JSON", raw: `{"messages":[]} true`, wantValid: false},
		{name: "valid numbers", raw: `{"a":-12.5e+3,"b":0,"messages":[]}`, wantValid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			media, valid := requestMayContainMedia([]byte(test.raw))
			if media != test.wantMedia || valid != test.wantValid {
				t.Fatalf("media=%v valid=%v, want %v/%v", media, valid, test.wantMedia, test.wantValid)
			}
		})
	}
}

func TestPureTextFastPathDoesNotAddProviderFields(t *testing.T) {
	runtime := testRuntime()
	defer runtime.cache.close()
	raw := []byte(`{"model":"glm-5.2-vision-combo","messages":[{"role":"user","content":"hello"}]}`)
	got, count, err := transformOpenAIRequest(raw, runtime, func(visualAsset, string) (string, error) {
		t.Fatal("vision should not run")
		return "", nil
	})
	if err != nil || count != 0 || string(got) != string(raw) {
		t.Fatalf("count=%d err=%v body=%s", count, err, got)
	}
	for _, field := range []string{"prompt_cache_key", "prompt_cache_options", "prompt_cache_retention", "previous_response_id"} {
		if containsJSONField(got, field) {
			t.Fatalf("provider-specific field %q was injected", field)
		}
	}
}

func containsJSONField(raw []byte, field string) bool {
	needle := []byte(`"` + field + `"`)
	for index := 0; index+len(needle) <= len(raw); index++ {
		matched := true
		for offset := range needle {
			if raw[index+offset] != needle[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
