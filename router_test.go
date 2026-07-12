package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testRuntime() runtimeConfig {
	cfg := defaultPluginConfig()
	normalized, _ := normalizeConfig(cfg)
	return runtimeConfig{pluginConfig: normalized, cache: newMemoCache(8, ""), events: newEventStore(100)}
}

func TestTransformOpenAIRequestReplacesImageAndPreservesText(t *testing.T) {
	raw := []byte(`{"model":"glm-5.2-vision-combo","messages":[{"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`)
	got, count, err := transformOpenAIRequest(raw, testRuntime(), func(a visualAsset, context string) (string, error) {
		if a.URL != "https://example.test/a.png" {
			t.Fatal(a.URL)
		}
		if !strings.Contains(context, "what is this?") {
			t.Fatal(context)
		}
		return "A blue square.", nil
	})
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if strings.Contains(string(got), "https://example.test/a.png") || !strings.Contains(string(got), "A blue square.") {
		t.Fatalf("unexpected: %s", got)
	}
}

func TestTransformRespectsRemoteURLPolicy(t *testing.T) {
	r := testRuntime()
	r.AllowRemoteImageURLs = false
	_, _, err := transformOpenAIRequest([]byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`), r, func(asset visualAsset, _ string) (string, error) { return "", validateAsset(asset.URL, r) })
	if err == nil {
		t.Fatal("expected URL policy error")
	}
}

func TestVisionRequestAndResponse(t *testing.T) {
	request := makeVisionRequest("vision", "prompt", "context", "data:image/png;base64,a", 100)
	var decoded map[string]any
	if err := json.Unmarshal(request, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["model"] != "vision(low)" || decoded["reasoning_effort"] != "low" {
		t.Fatal(decoded)
	}
	if got := extractVisionText([]byte(`{"choices":[{"message":{"content":"diagram: one box"}}]}`)); got != "diagram: one box" {
		t.Fatal(got)
	}
}

func TestTooManyNewImagesAreRejectedBeforeAnyVisionCall(t *testing.T) {
	r := testRuntime()
	r.MaxImagesPerRequest = 1
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}},{"type":"image_url","image_url":{"url":"https://example.test/b.png"}}]}]}`)
	calls := 0
	_, count, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "should not be called", nil
	})
	if err == nil || calls != 0 || count != 2 {
		t.Fatalf("count=%d calls=%d err=%v, want preflight rejection", count, calls, err)
	}
}

func TestCachedHistoricalImageIsCompactedWithoutVisionCall(t *testing.T) {
	r := testRuntime()
	r.MaxImagesPerRequest = 1
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},{"role":"assistant","content":"看到了"},{"role":"user","content":"继续讨论代码"}]}`)
	var root any
	_ = json.Unmarshal(raw, &root)
	asset := collectVisualAssets(root)[0]
	r.cache.set(visualCacheKey(r, asset), "vision", "cached visual memory with details", time.Hour)
	calls := 0
	got, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) { calls++; return "", nil })
	if err != nil || calls != 0 || !strings.Contains(string(got), "历史图片附件已归档") || strings.Contains(string(got), "data:image") {
		t.Fatalf("calls=%d err=%v body=%s", calls, err, got)
	}
}

func TestAllCachedHistoryImagesAreRewritten(t *testing.T) {
	r := testRuntime()
	r.MaxImagesPerRequest = 2
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/one.png"}},{"type":"image_url","image_url":{"url":"https://example.test/two.png"}}]}]}`)
	got, count, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) { return "cached visual memory", nil })
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if strings.Contains(string(got), `"image_url"`) || strings.Contains(string(got), "one.png") || strings.Contains(string(got), "two.png") {
		t.Fatalf("raw images were not fully replaced: %s", got)
	}
}

func TestNamedVisionChainOverridesAdvancedList(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.VisionPrimaryModel = "vision-a"
	cfg.VisionBackupModel1 = "vision-b"
	cfg.VisionContextLimit = 256000
	cfg.VisionModels = []visionModel{{Model: "ignored-advanced", ContextLimit: 1}}
	got, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.VisionModels) != 2 || got.VisionModels[0].Model != "vision-a" || got.VisionModels[1].Model != "vision-b" {
		t.Fatalf("unexpected chain: %#v", got.VisionModels)
	}
	if got.VisionModels[0].ContextLimit != 256000 {
		t.Fatalf("context limit = %d", got.VisionModels[0].ContextLimit)
	}
}

func TestEventStoreKeepsBoundedSanitizedHistory(t *testing.T) {
	store := newEventStore(1)
	first := store.begin("combo", "glm", false)
	store.stage(first, "视觉识别完成", "完成", "vision", strings.Repeat("x", 700), time.Now())
	store.finish(first, nil)
	_ = store.begin("combo", "glm", true)
	events := store.snapshot()
	if len(events) != 1 || !events[0].Stream {
		t.Fatalf("unexpected events: %#v", events)
	}
	if got := abbreviateEventText(strings.Repeat("x", 700), 20); !strings.HasSuffix(got, "…") {
		t.Fatalf("expected abbreviated text: %q", got)
	}
}

func TestCPALocalAPISettings(t *testing.T) {
	port, key := cpaLocalAPISettings([]byte("port: 9123\napi-keys:\n  - test-key\n"))
	if port != 9123 || key != "test-key" {
		t.Fatalf("settings = (%d, %q), want (9123, test-key)", port, key)
	}
	port, key = cpaLocalAPISettings([]byte("api-keys: []\n"))
	if port != defaultCPAManagementPort || key != "" {
		t.Fatalf("empty settings = (%d, %q)", port, key)
	}
}
