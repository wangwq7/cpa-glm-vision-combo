package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

func BenchmarkTransformPureTextLargeHistory(b *testing.B) {
	cfg := testRuntime()
	defer cfg.cache.close()
	items := make([]any, 0, 64)
	for index := 0; index < 64; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		items = append(items, map[string]any{"role": role, "content": fmt.Sprintf("%02d:%s", index, strings.Repeat("x", 32000))})
	}
	raw, _ := json.Marshal(map[string]any{"model": "glm-5.2-vision-combo", "messages": items})
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		got, count, err := transformOpenAIRequest(raw, cfg, func(visualAsset, string) (string, error) {
			b.Fatal("pure text benchmark called vision")
			return "", nil
		})
		if err != nil || count != 0 || len(got) != len(raw) {
			b.Fatalf("bytes=%d count=%d err=%v", len(got), count, err)
		}
	}
}

func BenchmarkPrepareFinalCheckpointReuse(b *testing.B) {
	var calls atomic.Int32
	cfg := testRuntime()
	defer cfg.cache.close()
	cfg.historySummarizer = func(string, runtimeConfig, string, *comboEvent) (string, error) {
		calls.Add(1)
		return "stable checkpoint summary", nil
	}
	huge := strings.Repeat("h", 1090000)
	items := []any{
		map[string]any{"role": "user", "content": huge},
		map[string]any{"role": "assistant", "content": huge},
	}
	for index := 0; index < 8; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		items = append(items, map[string]any{"role": role, "content": fmt.Sprintf("recent-%d", index)})
	}
	raw, _ := json.Marshal(map[string]any{"model": "glm-5.2-vision-combo", "messages": items})
	if _, err := prepareFinalTextBody(raw, cfg, "", nil); err != nil {
		b.Fatal(err)
	}
	if calls.Load() != 1 {
		b.Fatalf("seed calls=%d", calls.Load())
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		got, err := prepareFinalTextBody(raw, cfg, "", nil)
		if err != nil || len(got) >= len(raw) {
			b.Fatalf("bytes=%d err=%v", len(got), err)
		}
	}
	if calls.Load() != 1 {
		b.Fatalf("reuse called summarizer: %d", calls.Load())
	}
}

func BenchmarkTransformManyArchivedImages(b *testing.B) {
	cfg := testRuntime()
	defer cfg.cache.close()
	items := make([]any, 0, 81)
	payload := strings.Repeat("YQ==", 250000)
	for index := 0; index < 40; index++ {
		image := map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64," + payload},
			}},
		}
		items = append(items,
			image,
			map[string]any{"role": "assistant", "content": "seen"},
		)
	}
	items = append(items, map[string]any{"role": "user", "content": "continue discussing code"})
	raw, _ := json.Marshal(map[string]any{"model": "glm-5.2-vision-combo", "messages": items})
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		got, count, err := transformOpenAIRequest(raw, cfg, func(visualAsset, string) (string, error) {
			b.Fatal("archived image benchmark called vision")
			return "", nil
		})
		if err != nil || count != 40 || strings.Contains(string(got), "data:image") {
			b.Fatalf("bytes=%d count=%d err=%v", len(got), count, err)
		}
	}
}

func BenchmarkPrepareFinalLargeToolHistory(b *testing.B) {
	cfg := testRuntime()
	defer cfg.cache.close()
	cfg.AutoCompressionThresholdTokens = 720000
	cfg.PrimaryContextBudgetTokens = 930000
	cfg.AutoCompressionKeepRecentTurns = 8
	items := make([]any, 0, 100)
	items = append(items, map[string]any{"role": "system", "content": strings.Repeat("system rules ", 3000)})
	for index := 0; index < 24; index++ {
		id := fmt.Sprintf("call_%02d", index)
		items = append(items,
			map[string]any{"role": "user", "content": fmt.Sprintf("task %02d: inspect the repository", index)},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "exec", "arguments": strings.Repeat("a", 6900)}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": strings.Repeat("t", 24900)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("conclusion %02d: %s", index, strings.Repeat("c", 600))},
		)
	}
	items = append(items, map[string]any{"role": "user", "content": "latest follow-up"})
	raw, _ := json.Marshal(map[string]any{"model": "glm-5.2-vision-combo", "messages": items})
	got, err := prepareFinalTextBody(raw, cfg, "", nil)
	if err != nil {
		b.Fatal(err)
	}
	if len(got) >= len(raw)/4 {
		b.Fatalf("tool history did not shrink enough: before=%d after=%d", len(raw), len(got))
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		got, err := prepareFinalTextBody(raw, cfg, "", nil)
		if err != nil || len(got) >= len(raw)/4 {
			b.Fatalf("bytes=%d err=%v", len(got), err)
		}
	}
	b.ReportMetric(float64(len(raw)), "before_bytes")
	b.ReportMetric(float64(len(got)), "after_bytes")
}
