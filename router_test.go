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

func TestDefaultConfigUsesBenchmarkedProductionProfile(t *testing.T) {
	cfg, err := normalizeConfig(defaultPluginConfig())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PrimaryModel != "glm-5.2" || len(cfg.TextFallbackModels) != 2 || cfg.TextFallbackModels[0] != "gpt-5.5" || cfg.TextFallbackModels[1] != "gpt-5.6-sol" {
		t.Fatalf("unexpected text chain: primary=%q fallbacks=%#v", cfg.PrimaryModel, cfg.TextFallbackModels)
	}
	wantVision := []string{"gemini-3.1-flash-lite", "gpt-5.6-terra", "grok-4.5", "claude-sonnet-4-6"}
	if len(cfg.VisionModels) != len(wantVision) {
		t.Fatalf("unexpected visual chain: %#v", cfg.VisionModels)
	}
	for index, want := range wantVision {
		if cfg.VisionModels[index].Model != want {
			t.Fatalf("visual model %d = %q, want %q", index, cfg.VisionModels[index].Model, want)
		}
	}
	if cfg.VisionTimeoutSeconds != 20 || cfg.VisionCancelGraceSeconds != 15 {
		t.Fatalf("unexpected visual timing: timeout=%d grace=%d", cfg.VisionTimeoutSeconds, cfg.VisionCancelGraceSeconds)
	}
}

func TestDefaultVisionPromptRequiresVerbatimOCR(t *testing.T) {
	for _, instruction := range []string{"Accuracy has priority over brevity", "timestamp", "table cell verbatim", "Do not normalize", "keep values from separate UI regions separate", "instead of guessing"} {
		if !strings.Contains(defaultVisionPrompt, instruction) {
			t.Fatalf("default vision prompt is missing %q", instruction)
		}
	}
}

func TestVisionRequestUsesHighImageDetail(t *testing.T) {
	raw := makeVisionRequest("vision-a", defaultVisionPrompt, "读取截图", "data:image/png;base64,YQ==", 4000)
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	messages := request["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	image := content[1].(map[string]any)["image_url"].(map[string]any)
	if image["detail"] != "high" {
		t.Fatalf("image detail = %#v", image["detail"])
	}
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
	if decoded["model"] != "vision(low)" || decoded["reasoning_effort"] != "low" || decoded["stream"] != true {
		t.Fatal(decoded)
	}
	if got := extractVisionText([]byte(`{"choices":[{"message":{"content":"diagram: one box"}}]}`)); got != "diagram: one box" {
		t.Fatal(got)
	}
}

func TestVisionCancelGraceDefaultsAndCaps(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.VisionCancelGraceSeconds = 0
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.VisionCancelGraceSeconds != 15 {
		t.Fatalf("default grace = %d, want 15", normalized.VisionCancelGraceSeconds)
	}
	cfg.VisionCancelGraceSeconds = 999
	normalized, err = normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.VisionCancelGraceSeconds != 120 {
		t.Fatalf("capped grace = %d, want 120", normalized.VisionCancelGraceSeconds)
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
	context := trimToTokens(nearbyUserTask(root, asset), r.VisionInputTokenBudget)
	r.cache.set(visualCacheKey(r, asset, context), "vision", "cached visual memory with details", time.Hour)
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
	cfg.VisionBackupModel2 = ""
	cfg.VisionBackupModel3 = ""
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

func TestVisionChainCannotPointBackToCombo(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.VisionPrimaryModel = cfg.ComboModel
	_, err := normalizeConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot point back to this combo") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeRejectsTextVisionModelOverlap(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.PrimaryModel = "glm-5.2"
	cfg.TextFallbackModels = []string{"gpt-5.5", "gpt-5.6-terra"}
	cfg.VisionPrimaryModel = "gpt-5.4-mini"
	cfg.VisionBackupModel1 = "gpt-5.6-terra"
	if _, err := normalizeConfig(cfg); err == nil || !strings.Contains(err.Error(), "both text and visual") {
		t.Fatalf("expected text/vision overlap error, got %v", err)
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
