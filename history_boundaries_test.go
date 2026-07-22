package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func safeCompressionTestRuntime(t *testing.T) runtimeConfig {
	t.Helper()
	cfg := testRuntime()
	cfg.AutoCompressionEnabled = true
	cfg.AutoCompressionThresholdTokens = 1
	cfg.PrimaryContextBudgetTokens = 100000
	cfg.AutoCompressionTargetTokens = 50
	cfg.AutoCompressionKeepRecentTurns = 2
	cfg.historySummarizer = func(string, runtimeConfig, string, *comboEvent) (string, error) {
		return "safe checkpoint summary", nil
	}
	return cfg
}

func buildResponsesToolHistory(pairs int) []any {
	items := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "finish the task"}}}}
	for index := 0; index < pairs; index++ {
		callID := "call_" + string(rune('a'+index))
		items = append(items,
			map[string]any{"type": "function_call", "id": "fc_" + callID, "call_id": callID, "name": "shell_command", "arguments": `{"command":"inspect"}`},
			map[string]any{"type": "function_call_output", "call_id": callID, "output": "result-" + callID},
		)
	}
	return items
}

func buildChatToolHistory(pairs int) []any {
	items := []any{map[string]any{"role": "user", "content": "finish the task"}}
	for index := 0; index < pairs; index++ {
		callID := "call_" + string(rune('a'+index))
		items = append(items,
			map[string]any{"role": "assistant", "content": "continue", "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": "shell_command", "arguments": `{"command":"inspect"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "result-" + callID},
		)
	}
	return items
}

func buildClaudeToolHistory(pairs int) []any {
	items := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "finish the task"}}}}
	for index := 0; index < pairs; index++ {
		callID := "call_" + string(rune('a'+index))
		items = append(items,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "continue"}, map[string]any{"type": "tool_use", "id": callID, "name": "shell_command", "input": map[string]any{"command": "inspect"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": callID, "content": "result-" + callID}}},
		)
	}
	return items
}

func collectToolPairIDs(value any) (calls, results []string) {
	callSet := map[string]struct{}{}
	resultSet := map[string]struct{}{}
	var visit func(any)
	visit = func(current any) {
		switch item := current.(type) {
		case []any:
			for _, child := range item {
				visit(child)
			}
		case map[string]any:
			switch stringValue(item["type"]) {
			case "function_call", "tool_use":
				if id := historyToolCallID(item); id != "" {
					callSet[id] = struct{}{}
				}
			case "function_call_output", "tool_result":
				if id := historyToolResultID(item); id != "" {
					resultSet[id] = struct{}{}
				}
			}
			if stringValue(item["role"]) == "assistant" {
				if toolCalls, ok := item["tool_calls"].([]any); ok {
					for _, rawTool := range toolCalls {
						tool, _ := rawTool.(map[string]any)
						if id := stringValue(tool["id"]); id != "" {
							callSet[id] = struct{}{}
						}
					}
				}
			}
			if stringValue(item["role"]) == "tool" {
				if id := stringValue(item["tool_call_id"]); id != "" {
					resultSet[id] = struct{}{}
				}
			}
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(value)
	for id := range callSet {
		calls = append(calls, id)
	}
	for id := range resultSet {
		results = append(results, id)
	}
	sort.Strings(calls)
	sort.Strings(results)
	return calls, results
}

func TestHistoryCompressionPreservesToolPairsAcrossProtocols(t *testing.T) {
	tests := []struct {
		name  string
		field string
		items []any
	}{
		{name: "OpenAI Responses", field: "input", items: buildResponsesToolHistory(5)},
		{name: "OpenAI Chat", field: "messages", items: buildChatToolHistory(5)},
		{name: "Anthropic Messages", field: "messages", items: buildClaudeToolHistory(5)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := safeCompressionTestRuntime(t)
			defer cfg.cache.close()
			raw, err := json.Marshal(map[string]any{"model": "combo", test.field: test.items})
			if err != nil {
				t.Fatal(err)
			}
			got, err := prepareFinalTextBody(raw, cfg, "", cfg.events.begin("combo", "glm", false))
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]any
			if err := json.Unmarshal(got, &root); err != nil {
				t.Fatal(err)
			}
			calls, results := collectToolPairIDs(root[test.field])
			if !reflect.DeepEqual(calls, results) {
				t.Fatalf("tool calls/results differ: calls=%v results=%v body=%s", calls, results, got)
			}
			if !reflect.DeepEqual(calls, []string{"call_d", "call_e"}) {
				t.Fatalf("retained calls=%v, want the two latest complete transactions", calls)
			}
		})
	}
}

func TestHistoryCompressionBoundariesSkipSplitAndOpenTools(t *testing.T) {
	items := buildResponsesToolHistory(2)
	if got := historyCompressionBoundaries(items); !reflect.DeepEqual(got, []int{0, 3, 5}) {
		t.Fatalf("boundaries=%v, want [0 3 5]", got)
	}
	open := append(items, map[string]any{"type": "function_call", "id": "fc_open", "call_id": "call_open", "name": "shell_command", "arguments": `{}`})
	if historyBoundarySafe(open, len(open)) {
		t.Fatal("boundary after unfinished tool call was considered safe")
	}
	got := historyCompressionBoundaries(open)
	if !reflect.DeepEqual(got, []int{0, 3, 5}) {
		t.Fatalf("open-call boundaries=%v, want the unfinished call retained after boundary 5", got)
	}
}

func TestHistoryCompressionBoundariesDoNotSplitParallelTools(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": "complete the task"},
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "call_a", "type": "function", "function": map[string]any{"name": "shell_command", "arguments": `{}`}},
				map[string]any{"id": "call_b", "type": "function", "function": map[string]any{"name": "shell_command", "arguments": `{}`}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_a", "content": "result-a"},
		map[string]any{"role": "tool", "tool_call_id": "call_b", "content": "result-b"},
		map[string]any{"role": "user", "content": "continue"},
	}
	if historyBoundarySafe(items, 3) {
		t.Fatal("boundary between parallel tool results was considered safe")
	}
	if got := historyCompressionBoundaries(items); !reflect.DeepEqual(got, []int{0, 4, 5}) {
		t.Fatalf("parallel-tool boundaries=%v, want [0 4 5]", got)
	}
}

func TestClaudeToolResultDoesNotStartNewUserTurn(t *testing.T) {
	items := buildClaudeToolHistory(1)
	if historyItemStartsUserTurn(items[2]) {
		t.Fatal("Claude tool_result user message was treated as a new user turn")
	}
}
