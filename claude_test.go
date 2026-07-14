package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestPluginRegistersOpenAIAndClaudeProtocols(t *testing.T) {
	registration := pluginRegistration()
	if got := strings.Join(registration.Capabilities.ExecutorInputFormats, ","); got != "openai,claude" {
		t.Fatalf("input formats = %q", got)
	}
	if got := strings.Join(registration.Capabilities.ExecutorOutputFormats, ","); got != "openai,claude" {
		t.Fatalf("output formats = %q", got)
	}
}

func TestClaudeRouteIsHandled(t *testing.T) {
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rpcRouteRequest{ModelRouteRequest: pluginapi.ModelRouteRequest{
		SourceFormat:   "claude",
		RequestedModel: defaultPluginConfig().ComboModel,
	}})
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
	if !response.Handled || response.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("route response = %#v", response)
	}
}

func TestClaudeToolResultImageIsReplacedWithoutFlatteningHistory(t *testing.T) {
	runtime := testRuntime()
	raw := []byte(`{
		"model":"glm-5.2-vision-combo",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"请根据截图定位登录失败的原因"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_123","name":"computer","input":{"action":"screenshot"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[
				{"type":"text","text":"tool generated output must not become the user task"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}}
			]}]}
		]
	}`)
	got, count, err := transformRequest(raw, "claude", runtime, func(asset visualAsset, context string) (string, error) {
		if asset.URL != "data:image/png;base64,YQ==" {
			t.Fatalf("image URL = %q", asset.URL)
		}
		if !strings.Contains(context, "登录失败") || strings.Contains(context, "tool generated") {
			t.Fatalf("nearby task = %q", context)
		}
		return "SUMMARY: 登录页显示会话已过期", nil
	})
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	text := string(got)
	for _, want := range []string{"tool_result", "tool_use_id", "toolu_123", "会话已过期", "tool generated output"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for _, leaked := range []string{`"type":"image"`, `"source"`, "YQ=="} {
		if strings.Contains(text, leaked) {
			t.Fatalf("raw media leaked via %q in %s", leaked, text)
		}
	}
}

func TestVisualCacheKeyIncludesNearbyTask(t *testing.T) {
	runtime := testRuntime()
	asset := visualAsset{URL: "data:image/png;base64,YQ=="}
	first := visualCacheKey(runtime, asset, "读取错误代码")
	second := visualCacheKey(runtime, asset, "比较页面布局")
	if first == "" || second == "" || first == second {
		t.Fatalf("task-sensitive cache keys = %q %q", first, second)
	}
	if first != visualCacheKey(runtime, asset, "  读取错误代码  ") {
		t.Fatal("equivalent task whitespace should not fragment the cache")
	}
}

func TestUnsupportedMediaFailsBeforeAnyVisionCall(t *testing.T) {
	runtime := testRuntime()
	raw := []byte(`{"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}},
		{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}
	]}]}`)
	calls := 0
	_, count, err := transformRequest(raw, "claude", runtime, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "PDF") || count != 1 || calls != 0 {
		t.Fatalf("count=%d calls=%d err=%v", count, calls, err)
	}
}

func TestHistoricalPDFStillFailsClosed(t *testing.T) {
	runtime := testRuntime()
	raw := []byte(`{"messages":[
		{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}]},
		{"role":"assistant","content":"received"},
		{"role":"user","content":"continue without the attachment"}
	]}`)
	calls := 0
	_, _, err := transformRequest(raw, "claude", runtime, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "PDF") || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestMediaOutsideConversationFieldsFailsClosed(t *testing.T) {
	runtime := testRuntime()
	raw := []byte(`{
		"messages":[{"role":"user","content":"analyze"}],
		"attachment":{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}
	}`)
	calls := 0
	_, _, err := transformRequest(raw, "openai", runtime, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "outside messages/input") || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestFinalHostRequestPreservesClientProtocol(t *testing.T) {
	request := makeHostModelRequest("callback", "claude", "glm-5.2", []byte(`{"messages":[]}`), true)
	if request.EntryProtocol != "claude" || request.ExitProtocol != "claude" || !request.Stream {
		t.Fatalf("host request = %#v", request.HostModelExecutionRequest)
	}
}

func TestVisionSubrequestAlwaysUsesOpenAIProtocol(t *testing.T) {
	invoke := func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			request := payload.(hostModelRequest)
			if request.EntryProtocol != "openai" || request.ExitProtocol != "openai" {
				t.Fatalf("vision protocols = %q/%q", request.EntryProtocol, request.ExitProtocol)
			}
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "vision-openai"}), nil
		case pluginabi.MethodHostModelStreamRead:
			return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"), Done: true}), nil
		case pluginabi.MethodHostModelStreamClose:
			return streamJSON(map[string]any{}), nil
		default:
			t.Fatalf("unexpected method %s", method)
			return nil, nil
		}
	}
	text, err := executeVisionStreamWithTimeout("callback", "vision(low)", []byte(`{"stream":true}`), time.Second, time.Second, invoke)
	if err != nil || text != "ok" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}
