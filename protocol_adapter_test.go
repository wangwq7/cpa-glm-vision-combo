package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestProtocolResolutionUsesAuthoritativeHostMetadata(t *testing.T) {
	tests := []struct {
		name, source, format, raw, want string
	}{
		{
			name:   "source format wins over translated format",
			source: "openai-response",
			format: "openai",
			raw:    `{"messages":[{"role":"user","content":"translated payload shape must not override SourceFormat"}]}`,
			want:   protocolResponses,
		},
		{
			name:   "format is the fallback",
			format: "claude",
			raw:    `{"messages":[{"role":"user","content":"hello"}]}`,
			want:   protocolAnthropic,
		},
		{
			name:   "anthropic alias is canonicalized",
			source: "anthropic-messages",
			raw:    `{"messages":[{"role":"user","content":"hello"}]}`,
			want:   protocolAnthropic,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := resolveProtocolAdapter(test.source, test.format, []byte(test.raw))
			if err != nil || adapter.protocol != test.want {
				t.Fatalf("protocol=%q want=%q err=%v", adapter.protocol, test.want, err)
			}
		})
	}
}

func TestProtocolDetectionOnlyFallsBackForUnambiguousPayloads(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{
			name: "responses input",
			raw:  `{"input":[{"role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]}]}`,
			want: protocolResponses,
		},
		{
			name: "openai chat image",
			raw:  `{"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}]}`,
			want: protocolOpenAIChat,
		},
		{
			name: "anthropic image",
			raw:  `{"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}}]}]}`,
			want: protocolAnthropic,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := resolveProtocolAdapter("", "", []byte(test.raw))
			if err != nil || adapter.protocol != test.want {
				t.Fatalf("protocol=%q want=%q err=%v", adapter.protocol, test.want, err)
			}
		})
	}

	_, err := resolveProtocolAdapter("", "", []byte(`{"messages":[{"role":"user","content":"text only"}]}`))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("text-only detection err=%v", err)
	}
	_, err = resolveProtocolAdapter("", "", []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}},{"type":"image","source":{"type":"url","url":"https://example.test/b.png"}}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting detection err=%v", err)
	}
}

func TestProtocolAdaptersEmitNativeVisualMemoryBlocks(t *testing.T) {
	tests := []struct {
		name, protocol, raw, field string
		item, content              int
		wantType, removedType      string
	}{
		{
			name:        "openai chat",
			protocol:    protocolOpenAIChat,
			raw:         `{"model":"combo","messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}],"metadata":{"keep":"chat"}}`,
			field:       "messages",
			item:        0,
			content:     1,
			wantType:    "text",
			removedType: "image_url",
		},
		{
			name:        "responses",
			protocol:    protocolResponses,
			raw:         `{"model":"combo","input":[{"role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]}],"metadata":{"keep":"responses"}}`,
			field:       "input",
			item:        0,
			content:     1,
			wantType:    "input_text",
			removedType: "input_image",
		},
		{
			name:        "anthropic",
			protocol:    protocolAnthropic,
			raw:         `{"model":"combo","messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}}]}],"metadata":{"keep":"claude"}}`,
			field:       "messages",
			item:        0,
			content:     1,
			wantType:    "text",
			removedType: "image",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := testRuntime()
			defer runtime.cache.close()
			got, count, err := transformRequest([]byte(test.raw), test.protocol, runtime, func(visualAsset, string) (string, error) {
				return "recognized visual fact", nil
			})
			if err != nil || count != 1 {
				t.Fatalf("count=%d err=%v body=%s", count, err, got)
			}
			var root map[string]any
			if err := json.Unmarshal(got, &root); err != nil {
				t.Fatal(err)
			}
			items, _ := root[test.field].([]any)
			message, _ := items[test.item].(map[string]any)
			content, _ := message["content"].([]any)
			part, _ := content[test.content].(map[string]any)
			if part["type"] != test.wantType || !strings.Contains(stringValue(part["text"]), "recognized visual fact") {
				t.Fatalf("replacement=%#v body=%s", part, got)
			}
			if strings.Contains(string(got), `"type":"`+test.removedType+`"`) {
				t.Fatalf("original image type remained: %s", got)
			}
			metadata, _ := root["metadata"].(map[string]any)
			if metadata["keep"] == "" {
				t.Fatalf("unrelated metadata was lost: %s", got)
			}
		})
	}
}

func TestProtocolAdaptersRejectCrossProtocolImageBlocks(t *testing.T) {
	tests := []struct {
		protocol, raw string
	}{
		{
			protocol: protocolOpenAIChat,
			raw:      `{"messages":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]}]}`,
		},
		{
			protocol: protocolResponses,
			raw:      `{"input":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}]}`,
		},
		{
			protocol: protocolAnthropic,
			raw:      `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]}]}`,
		},
	}
	for _, test := range tests {
		_, _, err := transformRequest([]byte(test.raw), test.protocol, testRuntime(), func(visualAsset, string) (string, error) {
			return "unexpected", nil
		})
		if err == nil || !strings.Contains(err.Error(), "incompatible image block") {
			t.Fatalf("protocol=%s err=%v", test.protocol, err)
		}
	}
}

func TestExecutorProtocolUsesSourceFormatAndPayloadFallback(t *testing.T) {
	protocol, err := executorProtocol(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		SourceFormat:    protocolAnthropic,
		Format:          protocolOpenAIChat,
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
	}})
	if err != nil || protocol != protocolAnthropic {
		t.Fatalf("source protocol=%q err=%v", protocol, err)
	}
	protocol, err = executorProtocol(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		OriginalRequest: []byte(`{"input":"hello"}`),
	}})
	if err != nil || protocol != protocolResponses {
		t.Fatalf("detected protocol=%q err=%v", protocol, err)
	}
}

func TestRouteModelLeavesUnsupportedProtocolsUnhandled(t *testing.T) {
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(rpcRouteRequest{ModelRouteRequest: pluginapi.ModelRouteRequest{
		SourceFormat:   "gemini",
		RequestedModel: defaultPluginConfig().ComboModel,
	}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := routeModel(raw)
	if err != nil {
		t.Fatal(err)
	}
	var outer envelope
	if err := json.Unmarshal(encoded, &outer); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ModelRouteResponse
	if err := json.Unmarshal(outer.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.Handled {
		t.Fatalf("unsupported protocol was handled: %#v", response)
	}
}
