package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHasDeliverableModelResponseSupportsTextAndTools(t *testing.T) {
	for _, raw := range []string{
		`{"choices":[{"message":{"content":"answer"}}]}`,
		`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call_1","type":"function"}]}}]}`,
		`{"output":[{"type":"function_call","name":"exec","arguments":"{}"}]}`,
		`{"content":[{"type":"tool_use","id":"toolu_1","name":"exec","input":{}}]}`,
		`{"content":[{"type":"text","text":"claude answer"}]}`,
	} {
		if !hasDeliverableModelResponse([]byte(raw)) {
			t.Fatalf("expected deliverable response: %s", raw)
		}
	}
	for _, raw := range []string{
		``,
		`{"choices":[{"message":{"role":"assistant","content":""}}],"usage":{"total_tokens":10}}`,
		`{"output":[{"type":"reasoning","summary":[]}],"status":"completed"}`,
	} {
		if hasDeliverableModelResponse([]byte(raw)) {
			t.Fatalf("unexpected deliverable response: %s", raw)
		}
	}
}

func TestNonStreamingTextTruncationIsDetected(t *testing.T) {
	raw := []byte(`{"choices":[{"finish_reason":"length","message":{"content":"partial"}}]}`)
	if reason := responseTruncationReason(raw); reason != "finish_reason=length" {
		t.Fatalf("reason=%q", reason)
	}
}

func TestForwardPrimaryStreamDoesNotEmitMetadataBeforeFailure(t *testing.T) {
	reads := 0
	emits := 0
	invoke := func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "primary"}), nil
		case pluginabi.MethodHostModelStreamRead:
			reads++
			if reads == 1 {
				return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"choices":[{"delta":{"role":"assistant"}}]}`)}), nil
			}
			return nil, errors.New("upstream failed before content")
		case pluginabi.MethodHostStreamEmit:
			emits++
			return streamJSON(map[string]any{}), nil
		case pluginabi.MethodHostModelStreamClose:
			return streamJSON(map[string]any{}), nil
		default:
			return nil, errors.New("unexpected method")
		}
	}
	emitted, err := forwardPrimaryStreamWithHost("client", "callback", "primary", []byte(`{"stream":true}`), "openai", invoke)
	if err == nil || emitted || emits != 0 {
		t.Fatalf("emitted=%v emits=%d err=%v", emitted, emits, err)
	}
}

func TestForwardPrimaryStreamFlushesMetadataWhenContentArrives(t *testing.T) {
	reads := 0
	var emittedPayloads []string
	invoke := func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "primary"}), nil
		case pluginabi.MethodHostModelStreamRead:
			reads++
			switch reads {
			case 1:
				return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"choices":[{"delta":{"role":"assistant"}}]}`)}), nil
			case 2:
				return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"choices":[{"delta":{"content":"answer"}}]}`)}), nil
			default:
				return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte("data: [DONE]"), Done: true}), nil
			}
		case pluginabi.MethodHostStreamEmit:
			emittedPayloads = append(emittedPayloads, string(payload.(streamEmitRequest).Payload))
			return streamJSON(map[string]any{}), nil
		case pluginabi.MethodHostModelStreamClose:
			return streamJSON(map[string]any{}), nil
		default:
			return nil, errors.New("unexpected method")
		}
	}
	emitted, err := forwardPrimaryStreamWithHost("client", "callback", "primary", []byte(`{"stream":true}`), "openai", invoke)
	if err != nil || !emitted || len(emittedPayloads) != 3 {
		t.Fatalf("emitted=%v payloads=%v err=%v", emitted, emittedPayloads, err)
	}
	if !strings.Contains(emittedPayloads[0], `"role"`) || !strings.Contains(emittedPayloads[1], `"answer"`) || emittedPayloads[2] != "data: [DONE]" {
		t.Fatalf("payload order=%v", emittedPayloads)
	}
}

func TestForwardPrimaryStreamReportsEmissionWhenBufferedFlushPartlyFails(t *testing.T) {
	reads := 0
	emits := 0
	invoke := func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "primary"}), nil
		case pluginabi.MethodHostModelStreamRead:
			reads++
			if reads == 1 {
				return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"choices":[{"delta":{"role":"assistant"}}]}`)}), nil
			}
			return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"choices":[{"delta":{"content":"answer"}}]}`)}), nil
		case pluginabi.MethodHostStreamEmit:
			emits++
			if emits == 2 {
				return nil, errors.New("client stream write failed")
			}
			return streamJSON(map[string]any{}), nil
		case pluginabi.MethodHostModelStreamClose:
			return streamJSON(map[string]any{}), nil
		default:
			return nil, errors.New("unexpected method")
		}
	}
	emitted, err := forwardPrimaryStreamWithHost("client", "callback", "primary", []byte(`{"stream":true}`), "openai", invoke)
	if !emitted || err == nil || emits != 2 {
		t.Fatalf("emitted=%v emits=%d err=%v", emitted, emits, err)
	}
}

func TestForwardPrimaryStreamReportsTruncationAfterContent(t *testing.T) {
	reads := 0
	var emittedPayloads []string
	invoke := func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "primary"}), nil
		case pluginabi.MethodHostModelStreamRead:
			reads++
			if reads == 1 {
				return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"choices":[{"delta":{"content":"partial"}}]}`)}), nil
			}
			return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`), Done: true}), nil
		case pluginabi.MethodHostStreamEmit:
			emittedPayloads = append(emittedPayloads, string(payload.(streamEmitRequest).Payload))
			return streamJSON(map[string]any{}), nil
		case pluginabi.MethodHostModelStreamClose:
			return streamJSON(map[string]any{}), nil
		default:
			return nil, errors.New("unexpected method")
		}
	}
	emitted, err := forwardPrimaryStreamWithHost("client", "callback", "primary", []byte(`{"stream":true}`), "openai", invoke)
	if !emitted || err == nil || !strings.Contains(err.Error(), "truncated") || len(emittedPayloads) != 2 {
		t.Fatalf("emitted=%v payloads=%v err=%v", emitted, emittedPayloads, err)
	}
}

func TestForwardPrimaryStreamCanFallbackOnTruncationBeforeContent(t *testing.T) {
	invoke := func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostModelExecuteStream:
			return streamJSON(pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: "primary"}), nil
		case pluginabi.MethodHostModelStreamRead:
			return streamJSON(pluginapi.HostModelStreamReadResponse{Payload: []byte(`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`), Done: true}), nil
		case pluginabi.MethodHostModelStreamClose:
			return streamJSON(map[string]any{}), nil
		default:
			return nil, errors.New("unexpected method")
		}
	}
	emitted, err := forwardPrimaryStreamWithHost("client", "callback", "primary", []byte(`{"stream":true}`), "openai", invoke)
	if emitted || err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("emitted=%v err=%v", emitted, err)
	}
}

func TestStreamToolCallIsDeliverableOutput(t *testing.T) {
	gate := textStreamOutputGate{}
	ready, payloads, err := gate.add([]byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`))
	if err != nil || !ready || len(payloads) != 1 {
		t.Fatalf("ready=%v payloads=%d err=%v", ready, len(payloads), err)
	}
}

func TestCountTokensReturnsConservativeNonZeroEstimate(t *testing.T) {
	request, _ := json.Marshal(rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hello world"}]}`),
	}})
	raw, err := countTokens(request)
	if err != nil {
		t.Fatal(err)
	}
	var outer envelope
	if err := json.Unmarshal(raw, &outer); err != nil {
		t.Fatal(err)
	}
	var response pluginapi.ExecutorResponse
	if err := json.Unmarshal(outer.Result, &response); err != nil {
		t.Fatal(err)
	}
	var count map[string]int
	if err := json.Unmarshal(response.Payload, &count); err != nil {
		t.Fatal(err)
	}
	if count["input_tokens"] <= 0 {
		t.Fatalf("count response=%s", response.Payload)
	}
}

func TestTokenEstimateUsesImageReserveInsteadOfBase64Length(t *testing.T) {
	cfg := testRuntime()
	cfg.VisionImageTokenReserve = 4096
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("Y", 300000) + `"}}]}]}`)
	rawEstimate := estimateBodyTokens(body)
	got := estimateExecutorInputTokens(body, cfg)
	if got < 4096 || got >= rawEstimate/2 {
		t.Fatalf("image-aware estimate=%d raw estimate=%d", got, rawEstimate)
	}
}

func TestTokenEstimateArchivesUnreferencedHistoricalImages(t *testing.T) {
	cfg := testRuntime()
	items := make([]string, 0, 21)
	for index := 0; index < 10; index++ {
		items = append(items,
			`{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,`+strings.Repeat("Y", 30000)+`"}}]}`,
			`{"role":"assistant","content":"seen"}`,
		)
	}
	items = append(items, `{"role":"user","content":"continue discussing code"}`)
	body := []byte(`{"messages":[` + strings.Join(items, ",") + `]}`)
	got := estimateExecutorInputTokens(body, cfg)
	if got <= 0 || got >= cfg.VisionImageTokenReserve*2 {
		t.Fatalf("archived history estimate=%d", got)
	}
}

func TestTokenEstimateDoesNotPanicWhenCurrentImagesExceedLimit(t *testing.T) {
	cfg := testRuntime()
	cfg.MaxImagesPerRequest = 1
	body := []byte(`{"messages":[
		{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},
		{"role":"assistant","content":"seen"},
		{"role":"user","content":[
			{"type":"text","text":"compare this image with the previous image"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,Yg=="}},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,Yw=="}}
		]}
	]}`)
	if got := estimateExecutorInputTokens(body, cfg); got <= 0 {
		t.Fatalf("estimate=%d", got)
	}
}

func TestInvalidSSEDataDoesNotOpenTextFallbackGate(t *testing.T) {
	gate := textStreamOutputGate{}
	ready, _, err := gate.add([]byte("data: ping"))
	if err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
}

func TestVisibleReasoningOpensTextFallbackGate(t *testing.T) {
	for _, payload := range []string{
		`data: {"choices":[{"delta":{"reasoning_content":"working"}}]}`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"working"}`,
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"working"}}`,
	} {
		gate := textStreamOutputGate{}
		ready, _, err := gate.add([]byte(payload))
		if err != nil || !ready {
			t.Fatalf("payload=%s ready=%v err=%v", payload, ready, err)
		}
	}
}

func TestEmptyClaudeToolDeltaDoesNotOpenTextFallbackGate(t *testing.T) {
	gate := textStreamOutputGate{}
	ready, _, err := gate.add([]byte(`data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":""}}`))
	if err != nil || ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
}
