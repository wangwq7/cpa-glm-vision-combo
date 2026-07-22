package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func checkpointTestRuntime(calls *atomic.Int32) runtimeConfig {
	cfg := testRuntime()
	cfg.AutoCompressionThresholdTokens = 200
	cfg.PrimaryContextBudgetTokens = 500
	cfg.AutoCompressionTargetTokens = 50
	cfg.AutoCompressionKeepRecentTurns = 2
	cfg.historySummarizer = func(history string, _ runtimeConfig, _ string, _ *comboEvent) (string, error) {
		calls.Add(1)
		return "checkpoint summary: " + fmt.Sprint(len(history)), nil
	}
	return cfg
}

func TestHistoryCheckpointSurvivesCacheRestart(t *testing.T) {
	var calls atomic.Int32
	path := filepath.Join(t.TempDir(), "cache.json")
	first := checkpointTestRuntime(&calls)
	first.cache.close()
	first.cache = newMemoCache(100, path)
	raw := longHistoryBody(6, 300)
	if _, err := prepareFinalTextBody(raw, first, "", first.events.begin("combo", "glm", false)); err != nil {
		t.Fatal(err)
	}
	first.cache.close()

	second := checkpointTestRuntime(&calls)
	second.cache.close()
	second.cache = newMemoCache(100, path)
	defer second.cache.close()
	event := second.events.begin("combo", "glm", false)
	if _, err := prepareFinalTextBody(raw, second, "", event); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("persisted checkpoint was not reused; calls=%d", calls.Load())
	}
	assertEventStage(t, second.events, event.ID, "复用历史压缩检查点")
}

func TestResponsesInputHistoryReusesCheckpoint(t *testing.T) {
	var calls atomic.Int32
	cfg := checkpointTestRuntime(&calls)
	defer cfg.cache.close()
	items := make([]any, 0, 6)
	for index := 0; index < 6; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		items = append(items, map[string]any{
			"role": role,
			"content": []any{map[string]any{
				"type": "input_text",
				"text": fmt.Sprintf("%02d:%s", index, strings.Repeat("r", 300)),
			}},
		})
	}
	raw, _ := json.Marshal(map[string]any{"model": "glm-5.2-vision-combo", "input": items})
	if _, err := prepareFinalTextBody(raw, cfg, "", cfg.events.begin("combo", "glm", false)); err != nil {
		t.Fatal(err)
	}
	got, err := prepareFinalTextBody(raw, cfg, "", cfg.events.begin("combo", "glm", false))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(string(got), `"type":"input_text"`) || !strings.Contains(string(got), "checkpoint summary") {
		t.Fatalf("calls=%d body=%s", calls.Load(), got)
	}
}

func longHistoryBody(messages int, chars int) []byte {
	items := make([]any, 0, messages)
	for index := 0; index < messages; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		items = append(items, map[string]any{"role": role, "content": fmt.Sprintf("%02d:%s", index, strings.Repeat("x", chars))})
	}
	raw, _ := json.Marshal(map[string]any{"model": "glm-5.2-vision-combo", "messages": items})
	return raw
}

func appendHistoryMessages(t *testing.T, raw []byte, additions ...map[string]any) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	items := root["messages"].([]any)
	for _, item := range additions {
		items = append(items, item)
	}
	root["messages"] = items
	encoded, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestRepeatedLongHistoryReusesCheckpointWithoutSummarizingAgain(t *testing.T) {
	var calls atomic.Int32
	cfg := checkpointTestRuntime(&calls)
	defer cfg.cache.close()
	raw := longHistoryBody(6, 300)
	first, err := prepareFinalTextBody(raw, cfg, "", cfg.events.begin("combo", "glm", false))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !strings.Contains(string(first), "checkpoint summary") {
		t.Fatalf("first calls=%d body=%s", calls.Load(), first)
	}
	secondEvent := cfg.events.begin("combo", "glm", false)
	second, err := prepareFinalTextBody(raw, cfg, "", secondEvent)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || string(second) != string(first) {
		t.Fatalf("second calls=%d\nfirst=%s\nsecond=%s", calls.Load(), first, second)
	}
	assertEventStage(t, cfg.events, secondEvent.ID, "复用历史压缩检查点")
}

func TestTextContextPrecheckEventIsRecordedBelowCompressionThreshold(t *testing.T) {
	var calls atomic.Int32
	cfg := checkpointTestRuntime(&calls)
	defer cfg.cache.close()
	cfg.AutoCompressionThresholdTokens = 1000
	event := cfg.events.begin("combo", "glm", false)
	raw := longHistoryBody(2, 30)
	if _, err := prepareFinalTextBody(raw, cfg, "", event); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("summarizer calls=%d", calls.Load())
	}
	assertEventStage(t, cfg.events, event.ID, "文本上下文预检")
}

func TestToolTrajectoriesRemainByteIdenticalBelowCompressionThreshold(t *testing.T) {
	largeResult := strings.Repeat("tool-result-", 800)
	responsesInput := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "finish the task"}}}}
	chatMessages := []any{map[string]any{"role": "user", "content": "finish the task"}}
	claudeMessages := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "finish the task"}}}}
	for index := 0; index < 6; index++ {
		callID := fmt.Sprintf("call_%d", index)
		output := fmt.Sprintf("%s%d", largeResult, index)
		responsesInput = append(responsesInput,
			map[string]any{"type": "reasoning", "id": "reasoning_" + callID, "summary": []any{map[string]any{"type": "summary_text", "text": "continue"}}},
			map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": "shell_command", "arguments": `{"command":"inspect"}`},
			map[string]any{"type": "function_call_output", "call_id": callID, "output": output},
		)
		chatMessages = append(chatMessages,
			map[string]any{"role": "assistant", "content": "continue", "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "shell_command", "arguments": `{"command":"inspect"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": output},
		)
		claudeMessages = append(claudeMessages,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "continue"}, map[string]any{"type": "tool_use", "id": callID, "name": "shell_command", "input": map[string]any{"command": "inspect"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": output}}},
		)
	}

	encode := func(value map[string]any) []byte {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	tests := []struct {
		name     string
		protocol string
		raw      []byte
	}{
		{name: "OpenAI Chat", protocol: "openai", raw: encode(map[string]any{"model": "combo", "messages": chatMessages})},
		{name: "OpenAI Responses", protocol: "openai-response", raw: encode(map[string]any{"model": "combo", "input": responsesInput})},
		{name: "Anthropic Messages", protocol: "claude", raw: encode(map[string]any{"model": "combo", "messages": claudeMessages})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testRuntime()
			t.Cleanup(cfg.cache.close)
			cfg.AutoCompressionThresholdTokens = 1_000_000
			cfg.PrimaryContextBudgetTokens = 1_000_000
			got, err := prepareTextHostBody(test.raw, test.protocol, cfg, "", cfg.events.begin("combo", "glm", true))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(test.raw) {
				t.Fatalf("tool history changed below compression threshold: before=%d after=%d", len(test.raw), len(got))
			}
		})
	}
}

func TestFinalTextBodyRemovesOnlyTopLevelOutputLimits(t *testing.T) {
	cfg := testRuntime()
	defer cfg.cache.close()
	event := cfg.events.begin("combo", "glm", false)
	raw := []byte(`{
		"model":"glm-5.2-vision-combo",
		"max_tokens":50,
		"max_output_tokens":64,
		"max_completion_tokens":75,
		"reasoning_effort":"xhigh",
		"thinking":{"type":"enabled"},
		"messages":[{"role":"user","content":"solve"}],
		"tools":[{"type":"function","function":{"name":"run","parameters":{"type":"object","properties":{"max_tokens":{"type":"integer"},"max_output_tokens":{"type":"integer"},"max_completion_tokens":{"type":"integer"}}}}}]
	}`)

	got, err := prepareFinalTextBody(raw, cfg, "", event)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
		if _, exists := root[key]; exists {
			t.Fatalf("top-level %s was retained: %s", key, got)
		}
	}
	if root["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning_effort changed: %s", got)
	}
	thinking, _ := root["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking config changed: %s", got)
	}
	for _, nested := range []string{`"max_tokens":{"type":"integer"}`, `"max_output_tokens":{"type":"integer"}`, `"max_completion_tokens":{"type":"integer"}`} {
		if !strings.Contains(string(got), nested) {
			t.Fatalf("nested tool schema field %s was changed: %s", nested, got)
		}
	}
	assertEventStage(t, cfg.events, event.ID, "移除最终输出上限")
}

func TestFinalTextBodyWithoutTopLevelOutputLimitsRemainsByteIdentical(t *testing.T) {
	cfg := testRuntime()
	defer cfg.cache.close()
	raw := []byte(`{ "model": "glm-5.2-vision-combo", "reasoning_effort": "low", "messages": [{"role":"user","content":"hello"}], "tools":[{"parameters":{"properties":{"max_output_tokens":{"type":"integer"}}}}] }`)
	got, err := prepareFinalTextBody(raw, cfg, "", cfg.events.begin("combo", "glm", false))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("request without top-level output limits changed:\nwant=%s\ngot=%s", raw, got)
	}
}

func TestProtocolFinalTextBodiesRemoveClientOutputLimits(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		limitKey string
		raw      string
	}{
		{name: "OpenAI Chat max_tokens", protocol: "openai", limitKey: "max_tokens", raw: `{"model":"combo","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"metadata":{"trace":"keep-me"}}`},
		{name: "OpenAI Chat max_completion_tokens", protocol: "openai", limitKey: "max_completion_tokens", raw: `{"model":"combo","max_completion_tokens":64,"messages":[{"role":"user","content":"hello"}],"metadata":{"trace":"keep-me"}}`},
		{name: "OpenAI Responses", protocol: "openai-response", limitKey: "max_output_tokens", raw: `{"model":"combo","max_output_tokens":64,"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],"metadata":{"trace":"keep-me"}}`},
		{name: "Anthropic Messages", protocol: "claude", limitKey: "max_tokens", raw: `{"model":"combo","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"metadata":{"trace":"keep-me"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testRuntime()
			defer cfg.cache.close()
			got, err := prepareTextHostBody([]byte(test.raw), test.protocol, cfg, "", cfg.events.begin("combo", "glm", true))
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]any
			if err := json.Unmarshal(got, &root); err != nil {
				t.Fatal(err)
			}
			if _, exists := root[test.limitKey]; exists {
				t.Fatalf("%s retained %s: %s", test.protocol, test.limitKey, got)
			}
			metadata, _ := root["metadata"].(map[string]any)
			if metadata["trace"] != "keep-me" {
				t.Fatalf("%s metadata changed: %s", test.protocol, got)
			}
		})
	}
}

func TestHistoryCompressionRequestHasNoMaxTokens(t *testing.T) {
	raw := makeHistoryCompressionRequest("glm", `[{"role":"user","content":"history"}]`, 12000)
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	if _, exists := root["max_tokens"]; exists {
		t.Fatalf("compression request retained top-level max_tokens: %s", raw)
	}
	if root["stream"] != false || root["model"] != "glm" {
		t.Fatalf("unexpected compression request: %s", raw)
	}
}

func TestHistorySummaryTokenLimitAllowsSmallTokenizerVariance(t *testing.T) {
	if got := historySummaryTokenLimit(12000); got != 15000 {
		t.Fatalf("limit=%d, want 15000", got)
	}
	if got := historySummaryTokenLimit(50); got != 306 {
		t.Fatalf("small target limit=%d, want 306", got)
	}
}

func TestSmallAppendedTurnReusesCheckpointWithoutSummarizing(t *testing.T) {
	var calls atomic.Int32
	cfg := checkpointTestRuntime(&calls)
	defer cfg.cache.close()
	raw := longHistoryBody(6, 300)
	if _, err := prepareFinalTextBody(raw, cfg, "", cfg.events.begin("combo", "glm", false)); err != nil {
		t.Fatal(err)
	}
	appended := appendHistoryMessages(t, raw,
		map[string]any{"role": "user", "content": "small follow-up"},
		map[string]any{"role": "assistant", "content": "small answer"},
	)
	got, err := prepareFinalTextBody(appended, cfg, "", cfg.events.begin("combo", "glm", false))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("summarizer calls=%d, want 1", calls.Load())
	}
	for _, want := range []string{"checkpoint summary", "small follow-up", "small answer"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}

func TestCheckpointUpdatesOnceAfterDeltaExceedsBudget(t *testing.T) {
	var calls atomic.Int32
	cfg := checkpointTestRuntime(&calls)
	defer cfg.cache.close()
	raw := longHistoryBody(6, 300)
	if _, err := prepareFinalTextBody(raw, cfg, "", cfg.events.begin("combo", "glm", false)); err != nil {
		t.Fatal(err)
	}
	appended := appendHistoryMessages(t, raw,
		map[string]any{"role": "user", "content": strings.Repeat("a", 300)},
		map[string]any{"role": "assistant", "content": strings.Repeat("b", 300)},
		map[string]any{"role": "user", "content": strings.Repeat("c", 300)},
		map[string]any{"role": "assistant", "content": strings.Repeat("d", 300)},
	)
	event := cfg.events.begin("combo", "glm", false)
	if _, err := prepareFinalTextBody(appended, cfg, "", event); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("summarizer calls=%d, want exactly 2", calls.Load())
	}
	assertEventStage(t, cfg.events, event.ID, "更新历史压缩检查点")
	if _, err := prepareFinalTextBody(appended, cfg, "", cfg.events.begin("combo", "glm", false)); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("updated checkpoint was not reused; calls=%d", calls.Load())
	}
}

func assertEventStage(t *testing.T, store *eventStore, eventID, name string) {
	t.Helper()
	for _, event := range store.snapshot() {
		if event.ID != eventID {
			continue
		}
		for _, stage := range event.Stages {
			if stage.Name == name {
				return
			}
		}
	}
	t.Fatalf("event %s is missing stage %q", eventID, name)
}
