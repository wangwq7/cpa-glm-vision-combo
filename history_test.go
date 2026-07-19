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
