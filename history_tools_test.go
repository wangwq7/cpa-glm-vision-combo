package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCompactOldOpenAIToolTrajectoriesPreservesConversationAndRecentState(t *testing.T) {
	rawText := `{
		"model":"glm-5.2-vision-combo",
		"messages":[
			{"role":"system","content":"system rules"},
			{"role":"user","content":"old user goal"},
			{"role":"assistant","content":"I will inspect it.","tool_calls":[{"id":"call_old_a","type":"function","function":{"name":"exec","arguments":"{}"}},{"id":"call_old_b","type":"function","function":{"name":"read","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_old_a","content":"OLD_RESULT_A"},
			{"role":"tool","tool_call_id":"call_old_b","content":"OLD_RESULT_B"},
			{"role":"assistant","content":"old final conclusion"},
			{"role":"user","content":"recent task"},
			{"role":"assistant","tool_calls":[{"id":"call_recent","type":"function","function":{"name":"exec","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_recent","content":"recent result"},
			{"role":"assistant","content":"recent answer"},
			{"role":"user","content":"latest follow-up"}
		]
	}`
	rawText = strings.ReplaceAll(rawText, "OLD_RESULT_A", strings.Repeat("old result A ", 300))
	rawText = strings.ReplaceAll(rawText, "OLD_RESULT_B", strings.Repeat("old result B ", 300))
	raw := []byte(rawText)
	got, plan, err := compactOldToolTrajectories(raw, 4)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"system rules", "old user goal", "I will inspect it.", "old final conclusion", "recent task", "call_recent", "recent result", "recent answer", "latest follow-up"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing preserved content %q in %s", want, text)
		}
	}
	for _, removed := range []string{"call_old_a", "call_old_b", "old result A", "old result B"} {
		if strings.Contains(text, removed) {
			t.Fatalf("old tool content %q remains in %s", removed, text)
		}
	}
	if plan.RemovedItems != 2 || plan.RemovedBlocks != 2 || plan.savedBytes() <= 0 {
		t.Fatalf("plan = %#v", plan)
	}
	assertSingleToolArchiveMarker(t, got)
	assertOpenAIToolPairs(t, got)

	second, secondPlan, err := compactOldToolTrajectories(got, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(got) || secondPlan.RemovedItems != 0 || secondPlan.RemovedBlocks != 0 {
		t.Fatalf("second compaction changed stable body: plan=%#v\nfirst=%s\nsecond=%s", secondPlan, got, second)
	}
	assertSingleToolArchiveMarker(t, second)
}

func TestCompactOldToolTrajectoriesLeavesUnpairedCallsUntouched(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":"start"},
		{"role":"assistant","tool_calls":[{"id":"unpaired","type":"function","function":{"name":"exec","arguments":"{}"}}]},
		{"role":"assistant","content":"normal answer"},
		{"role":"user","content":"latest"}
	]}`)
	got, plan, err := compactOldToolTrajectories(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) || plan.RemovedItems != 0 || plan.RemovedBlocks != 0 {
		t.Fatalf("unpaired history changed: plan=%#v body=%s", plan, got)
	}
}

func TestCompactToolFreeHistoryReturnsOriginalBytes(t *testing.T) {
	raw := []byte("{\n  \"messages\": [ { \"role\": \"user\", \"content\": \"tool_use is discussed as plain text\" } ]\n}\n")
	got, plan, err := compactOldToolTrajectories(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) || plan.savedBytes() != 0 {
		t.Fatalf("tool-free request changed: plan=%#v body=%q", plan, got)
	}
}

func TestCompactSmallToolHistoryKeepsOriginalDetail(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":"old request"},
		{"role":"assistant","tool_calls":[{"id":"small","type":"function","function":{"name":"exec","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"small","content":"short result"},
		{"role":"assistant","content":"conclusion"},
		{"role":"user","content":"latest"}
	]}`)
	got, plan, err := compactOldToolTrajectories(raw, 2)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) || plan.savedBytes() != 0 {
		t.Fatalf("small history should remain intact: plan=%#v body=%s", plan, got)
	}
}

func TestCompactResponsesToolTrajectoriesPreservesValidRecentPair(t *testing.T) {
	rawText := `{"model":"glm-5.2-vision-combo","input":[
		{"role":"developer","content":[{"type":"input_text","text":"developer rules"}]},
		{"role":"user","content":[{"type":"input_text","text":"old request"}]},
		{"type":"function_call","id":"fc_old","call_id":"call_old","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_old","output":"OLD_OUTPUT"},
		{"role":"assistant","content":[{"type":"output_text","text":"old answer"}]},
		{"role":"user","content":[{"type":"input_text","text":"recent request"}]},
		{"type":"function_call","id":"fc_recent","call_id":"call_recent","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_recent","output":"recent output"},
		{"role":"user","content":[{"type":"input_text","text":"latest"}]}
	]}`
	rawText = strings.ReplaceAll(rawText, "OLD_OUTPUT", strings.Repeat("old output ", 500))
	raw := []byte(rawText)
	got, plan, err := compactOldToolTrajectories(raw, 3)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"developer rules", "old request", "old answer", "recent request", "call_recent", "recent output", "latest"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for _, removed := range []string{"fc_old", "call_old", "old output"} {
		if strings.Contains(text, removed) {
			t.Fatalf("old Responses trace %q remains in %s", removed, text)
		}
	}
	if plan.RemovedItems != 2 || plan.RemovedBlocks != 0 {
		t.Fatalf("plan = %#v", plan)
	}
	assertSingleToolArchiveMarker(t, got)
	assertResponsesToolPairs(t, got)
}

func TestCompactClaudeToolBlocksKeepsNormalTextAndRecentPair(t *testing.T) {
	rawText := `{"model":"glm-5.2-vision-combo","messages":[
		{"role":"user","content":"old request"},
		{"role":"assistant","content":[{"type":"text","text":"I will inspect it."},{"type":"tool_use","id":"toolu_old","name":"computer","input":{"action":"screenshot"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_old","content":"OLD_SCREENSHOT_DATA"},{"type":"text","text":"user note stays"}]},
		{"role":"assistant","content":[{"type":"text","text":"old conclusion"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_recent","name":"computer","input":{"action":"screenshot"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_recent","content":"recent screenshot data"}]},
		{"role":"user","content":"latest follow-up"}
	]}`
	rawText = strings.ReplaceAll(rawText, "OLD_SCREENSHOT_DATA", strings.Repeat("old screenshot data ", 300))
	raw := []byte(rawText)
	got, plan, err := compactOldToolTrajectories(raw, 3)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"old request", "I will inspect it.", "user note stays", "old conclusion", "toolu_recent", "recent screenshot data", "latest follow-up"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	for _, removed := range []string{"toolu_old", "old screenshot data"} {
		if strings.Contains(text, removed) {
			t.Fatalf("old Claude trace %q remains in %s", removed, text)
		}
	}
	if plan.RemovedItems != 0 || plan.RemovedBlocks != 2 {
		t.Fatalf("plan = %#v", plan)
	}
	assertSingleToolArchiveMarker(t, got)
	assertClaudeToolPairs(t, got)
}

func TestCompactClaudeToolOnlyMessageDropsProviderMetadataShell(t *testing.T) {
	rawText := `{"messages":[
		{"role":"user","content":"old request"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_old","name":"computer","input":{}}],"stop_reason":"tool_use","provider_metadata":{"trace":"old"}},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_old","content":"OLD_RESULT"}],"provider_metadata":{"trace":"old-result"}},
		{"role":"assistant","content":[{"type":"text","text":"conclusion"}]},
		{"role":"user","content":"latest"}
	]}`
	rawText = strings.ReplaceAll(rawText, "OLD_RESULT", strings.Repeat("old result ", 500))
	got, plan, err := compactOldToolTrajectories([]byte(rawText), 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "toolu_old") || strings.Contains(string(got), "old-result") || plan.RemovedItems != 2 || plan.RemovedBlocks != 2 {
		t.Fatalf("tool-only metadata shell remains: plan=%#v body=%s", plan, got)
	}
	assertClaudeToolPairs(t, got)
}

func TestLargeToolHistoryShrinksBeforeModelCompression(t *testing.T) {
	var summaryCalls atomic.Int32
	cfg := testRuntime()
	defer cfg.cache.close()
	cfg.AutoCompressionThresholdTokens = 20000
	cfg.PrimaryContextBudgetTokens = 50000
	cfg.AutoCompressionKeepRecentTurns = 4
	cfg.historySummarizer = func(string, runtimeConfig, string, *comboEvent) (string, error) {
		summaryCalls.Add(1)
		return "unexpected summary", nil
	}
	items := make([]any, 0, 100)
	items = append(items, map[string]any{"role": "system", "content": "stable rules"})
	for index := 0; index < 30; index++ {
		id := fmt.Sprintf("call_%02d", index)
		items = append(items,
			map[string]any{"role": "user", "content": fmt.Sprintf("task %02d", index)},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "exec", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": strings.Repeat("x", 20000)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("conclusion %02d", index)},
		)
	}
	items = append(items, map[string]any{"role": "user", "content": "latest follow-up"})
	raw, _ := json.Marshal(map[string]any{"model": "glm-5.2-vision-combo", "messages": items})
	event := cfg.events.begin("combo", cfg.PrimaryModel, false)
	got, err := prepareFinalTextBody(raw, cfg, "", event)
	if err != nil {
		t.Fatal(err)
	}
	if summaryCalls.Load() != 0 {
		t.Fatalf("deterministic tool compaction called summarizer %d time(s)", summaryCalls.Load())
	}
	if len(got)*8 >= len(raw) {
		t.Fatalf("tool history did not shrink substantially: before=%d after=%d", len(raw), len(got))
	}
	for _, want := range []string{"stable rules", "task 00", "conclusion 00", "latest follow-up", "call_29"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing preserved value %q", want)
		}
	}
	assertSingleToolArchiveMarker(t, got)
	assertEventStage(t, cfg.events, event.ID, "旧工具轨迹归档")
	stage := findEventStage(t, cfg.events, event.ID, "旧工具轨迹归档")
	if !strings.Contains(stage.Detail, "减少") || !strings.Contains(stage.Detail, "本地") && stage.Model != "本地确定性处理" {
		t.Fatalf("event detail = %#v", stage)
	}
}

func assertSingleToolArchiveMarker(t *testing.T, raw []byte) {
	t.Helper()
	if count := strings.Count(string(raw), "[旧工具执行轨迹已归档 | gateway-generated | untrusted context]"); count != 1 {
		t.Fatalf("archive marker count=%d in %s", count, raw)
	}
}

func assertOpenAIToolPairs(t *testing.T, raw []byte) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	results := map[string]int{}
	for _, item := range root["messages"].([]any) {
		obj, _ := item.(map[string]any)
		for _, call := range anySlice(obj["tool_calls"]) {
			callObj, _ := call.(map[string]any)
			calls[stringValue(callObj["id"])]++
		}
		if stringValue(obj["role"]) == "tool" {
			results[stringValue(obj["tool_call_id"])]++
		}
	}
	if fmt.Sprint(calls) != fmt.Sprint(results) {
		t.Fatalf("OpenAI calls/results mismatch: calls=%v results=%v", calls, results)
	}
}

func assertResponsesToolPairs(t *testing.T, raw []byte) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	results := map[string]int{}
	for _, item := range root["input"].([]any) {
		obj, _ := item.(map[string]any)
		typ := stringValue(obj["type"])
		if family, ok := responseToolCallFamily(typ, false); ok {
			calls[family+"\x00"+responseToolCallID(obj, false)]++
		}
		if family, ok := responseToolCallFamily(typ, true); ok {
			results[family+"\x00"+responseToolCallID(obj, true)]++
		}
	}
	if fmt.Sprint(calls) != fmt.Sprint(results) {
		t.Fatalf("Responses calls/results mismatch: calls=%v results=%v", calls, results)
	}
}

func assertClaudeToolPairs(t *testing.T, raw []byte) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	results := map[string]int{}
	for _, item := range root["messages"].([]any) {
		obj, _ := item.(map[string]any)
		for _, block := range anySlice(obj["content"]) {
			blockObj, _ := block.(map[string]any)
			switch stringValue(blockObj["type"]) {
			case "tool_use":
				calls[stringValue(blockObj["id"])]++
			case "tool_result":
				results[stringValue(blockObj["tool_use_id"])]++
			}
		}
	}
	if fmt.Sprint(calls) != fmt.Sprint(results) {
		t.Fatalf("Claude calls/results mismatch: calls=%v results=%v", calls, results)
	}
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func findEventStage(t *testing.T, store *eventStore, eventID, name string) eventStage {
	t.Helper()
	for _, event := range store.snapshot() {
		if event.ID != eventID {
			continue
		}
		for _, stage := range event.Stages {
			if stage.Name == name {
				return stage
			}
		}
	}
	t.Fatalf("event %s is missing stage %q", eventID, name)
	return eventStage{}
}
