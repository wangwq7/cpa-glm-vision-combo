package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
	for _, instruction := range []string{"starts directly", "Do not add a SUMMARY section by default", "never repeat", "timestamps", "table cells verbatim", "Do not normalize", "keep values from separate UI regions separate", "instead of guessing", "concise elsewhere"} {
		if !strings.Contains(defaultVisionPrompt, instruction) {
			t.Fatalf("default vision prompt is missing %q", instruction)
		}
	}
}

func TestHistoricalImageReferenceDetectionIsExplicit(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{text: "请重新查看上图", want: 1},
		{text: "分析这张截图", want: 1},
		{text: "compare the previous image with this result", want: 1},
		{text: "比较这两张图片", want: 3},
		{text: "review all screenshots", want: 3},
		{text: "图片多了以后为什么会变慢", want: 0},
		{text: "继续处理这个文件", want: 0},
		{text: "document the image cache behavior", want: 0},
		{text: "继续分析代码", want: 0},
	}
	for _, test := range tests {
		if got := historicalImageRestoreCount(test.text, 3); got != test.want {
			t.Fatalf("text=%q restore=%d, want %d", test.text, got, test.want)
		}
	}
}

func TestVisionRequestUsesHighImageDetail(t *testing.T) {
	raw := makeVisionRequest("vision-a", defaultVisionPrompt, "读取截图", "data:image/png;base64,YQ==")
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

func TestOversizedBase64ValidationStopsAtConfiguredLimit(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	r.MaxImageDataBytes = 3
	err := validateAsset("data:image/png;base64,"+strings.Repeat("YQ==", 100000), r)
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("err=%v", err)
	}
}

func TestVisionRequestAndResponse(t *testing.T) {
	request := makeVisionRequest("vision", "prompt", "context", "data:image/png;base64,a")
	var decoded map[string]any
	if err := json.Unmarshal(request, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["model"] != "vision(low)" || decoded["reasoning_effort"] != "low" || decoded["stream"] != true {
		t.Fatal(decoded)
	}
	if _, exists := decoded["max_tokens"]; exists {
		t.Fatalf("visual request retained top-level max_tokens: %s", request)
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

func TestLegacyVisionOutputTokenFieldsAreIgnored(t *testing.T) {
	raw := []byte(`
enabled: true
combo_model: combo
primary_model: text
vision_primary_model: vision
vision_output_tokens: 4000
vision_models:
  - model: ignored-because-legacy-fields-win
    max_output_tokens: 2000
`)
	var cfg pluginConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.VisionModels) != 1 || normalized.VisionModels[0].Model != "vision" {
		t.Fatalf("vision models=%#v", normalized.VisionModels)
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

func TestGenericFileFollowUpDoesNotRestoreHistoricalImage(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},{"role":"assistant","content":"看到了"},{"role":"user","content":"继续处理这个文件"}]}`)
	calls := 0
	got, count, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err != nil || count != 1 || calls != 0 || !strings.Contains(string(got), "历史图片附件已归档") {
		t.Fatalf("count=%d calls=%d err=%v body=%s", count, calls, err, got)
	}
}

func TestSingularAndPluralHistoryReferencesRestoreExpectedImages(t *testing.T) {
	rawTemplate := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}}]},{"role":"assistant","content":"第一张"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,Yg=="}}]},{"role":"assistant","content":"第二张"},{"role":"user","content":%q}]}`
	for _, test := range []struct {
		text string
		want int
	}{
		{text: "请重新查看上图", want: 1},
		{text: "请比较这两张图片", want: 2},
	} {
		r := testRuntime()
		r.HistoryRestoreMaxAttachments = 2
		var calls atomic.Int32
		raw := []byte(fmt.Sprintf(rawTemplate, test.text))
		_, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
			calls.Add(1)
			return "restored", nil
		})
		r.cache.close()
		if err != nil || calls.Load() != int32(test.want) {
			t.Fatalf("text=%q calls=%d err=%v, want %d", test.text, calls.Load(), err, test.want)
		}
	}
}

func TestManyUnreferencedHistoricalImagesSkipDecodeAndStayCompact(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	items := make([]any, 0, 81)
	for index := 0; index < 40; index++ {
		image := map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": fmt.Sprintf("data:image/png;base64,not-valid-%d", index)},
			}},
		}
		items = append(items,
			image,
			map[string]any{"role": "assistant", "content": "seen"},
		)
	}
	items = append(items, map[string]any{"role": "user", "content": "continue discussing the code"})
	raw, _ := json.Marshal(map[string]any{"messages": items})
	calls := 0
	got, count, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err != nil || count != 40 || calls != 0 {
		t.Fatalf("count=%d calls=%d err=%v", count, calls, err)
	}
	text := string(got)
	if strings.Count(text, "历史图片附件已归档") != 1 || strings.Contains(text, "not-valid-") || len(got) > 8000 {
		t.Fatalf("historical image output was not bounded: bytes=%d body=%s", len(got), got)
	}
}

func TestReferencedHistoricalImageIsStillStrictlyValidated(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,not-valid"}}]},{"role":"assistant","content":"seen"},{"role":"user","content":"请重新查看上图"}]}`)
	calls := 0
	_, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid base64") || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestHistoricalImageBlockWithPDFMediaTypeStillFailsClosed(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:application/pdf;base64,JVBERi0="}}]},{"role":"assistant","content":"seen"},{"role":"user","content":"continue the code discussion"}]}`)
	calls := 0
	_, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		calls++
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "PDF") || calls != 0 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestHistoricalRemotePDFImageURLStillFailsClosed(t *testing.T) {
	r := testRuntime()
	defer r.cache.close()
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.test/archive.pdf"}}]},{"role":"assistant","content":"seen"},{"role":"user","content":"continue the code discussion"}]}`)
	_, _, err := transformOpenAIRequest(raw, r, func(visualAsset, string) (string, error) {
		return "unexpected", nil
	})
	if err == nil || !strings.Contains(err.Error(), "PDF") {
		t.Fatalf("err=%v", err)
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
