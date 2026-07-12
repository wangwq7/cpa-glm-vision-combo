package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

const defaultVisionPrompt = `You are a precise visual preprocessing service. Analyze the supplied image for a downstream text-only reasoning model. Return compact, factual Markdown. Include: visible text/OCR, objects, layout, charts/tables with values, code, visual relationships, and uncertainty. Do not answer the user's broader request and do not invent details.`

type visionModel struct {
	Model        string `yaml:"model" json:"model"`
	ContextLimit int    `yaml:"context_limit" json:"context_limit"`
	Enabled      *bool  `yaml:"enabled" json:"enabled"`
}

func (m visionModel) active() bool { return m.Enabled == nil || *m.Enabled }

type pluginConfig struct {
	Enabled                 bool          `yaml:"enabled"`
	ComboModel              string        `yaml:"combo_model"`
	PrimaryModel            string        `yaml:"primary_model"`
	VisionPrimaryModel      string        `yaml:"vision_primary_model"`
	VisionBackupModel1      string        `yaml:"vision_backup_model_1"`
	VisionBackupModel2      string        `yaml:"vision_backup_model_2"`
	VisionBackupModel3      string        `yaml:"vision_backup_model_3"`
	VisionContextLimit      int           `yaml:"vision_context_limit"`
	VisionModels            []visionModel `yaml:"vision_models"`
	VisionPrompt            string        `yaml:"vision_prompt"`
	VisionInputTokenBudget  int           `yaml:"vision_input_token_budget"`
	VisionOutputTokens      int           `yaml:"vision_output_tokens"`
	VisionImageTokenReserve int           `yaml:"vision_image_token_reserve"`
	VisionTimeoutSeconds    int           `yaml:"vision_timeout_seconds"`
	CacheTTLSeconds         int           `yaml:"cache_ttl_seconds"`
	CacheMaxEntries         int           `yaml:"cache_max_entries"`
	EventLogMaxEntries      int           `yaml:"event_log_max_entries"`
	OnVisionFailure         string        `yaml:"on_vision_failure"`
	MaxImagesPerRequest     int           `yaml:"max_images_per_request"`
	MaxImageDataBytes       int           `yaml:"max_image_data_bytes"`
	AllowRemoteImageURLs    bool          `yaml:"allow_remote_image_urls"`
}

type runtimeConfig struct {
	pluginConfig
	cache  *memoCache
	events *eventStore
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:                 true,
		ComboModel:              "glm-5.2-vision-combo",
		PrimaryModel:            "glm-5.2",
		VisionPrompt:            defaultVisionPrompt,
		VisionInputTokenBudget:  24000,
		VisionOutputTokens:      4000,
		VisionImageTokenReserve: 4096,
		VisionContextLimit:      262144,
		VisionTimeoutSeconds:    45,
		CacheTTLSeconds:         86400,
		CacheMaxEntries:         512,
		EventLogMaxEntries:      100,
		OnVisionFailure:         "error",
		MaxImagesPerRequest:     8,
		MaxImageDataBytes:       12 * 1024 * 1024,
		AllowRemoteImageURLs:    true,
	}
}

func normalizeConfig(cfg pluginConfig) (pluginConfig, error) {
	def := defaultPluginConfig()
	if strings.TrimSpace(cfg.ComboModel) == "" {
		cfg.ComboModel = def.ComboModel
	}
	if strings.TrimSpace(cfg.PrimaryModel) == "" {
		cfg.PrimaryModel = def.PrimaryModel
	}
	if strings.TrimSpace(cfg.VisionPrompt) == "" {
		cfg.VisionPrompt = def.VisionPrompt
	}
	if cfg.VisionInputTokenBudget <= 0 {
		cfg.VisionInputTokenBudget = def.VisionInputTokenBudget
	}
	if cfg.VisionOutputTokens <= 0 {
		cfg.VisionOutputTokens = def.VisionOutputTokens
	}
	if cfg.VisionImageTokenReserve <= 0 {
		cfg.VisionImageTokenReserve = def.VisionImageTokenReserve
	}
	if cfg.VisionContextLimit <= 0 {
		cfg.VisionContextLimit = def.VisionContextLimit
	}
	if cfg.VisionTimeoutSeconds <= 0 {
		cfg.VisionTimeoutSeconds = def.VisionTimeoutSeconds
	}
	if cfg.CacheTTLSeconds <= 0 {
		cfg.CacheTTLSeconds = def.CacheTTLSeconds
	}
	if cfg.CacheMaxEntries <= 0 {
		cfg.CacheMaxEntries = def.CacheMaxEntries
	}
	if cfg.EventLogMaxEntries <= 0 {
		cfg.EventLogMaxEntries = def.EventLogMaxEntries
	}
	if cfg.MaxImagesPerRequest <= 0 {
		cfg.MaxImagesPerRequest = def.MaxImagesPerRequest
	}
	if cfg.MaxImageDataBytes <= 0 {
		cfg.MaxImageDataBytes = def.MaxImageDataBytes
	}
	cfg.ComboModel = strings.TrimSpace(cfg.ComboModel)
	cfg.PrimaryModel = strings.TrimSpace(cfg.PrimaryModel)
	cfg.VisionPrimaryModel = strings.TrimSpace(cfg.VisionPrimaryModel)
	cfg.VisionBackupModel1 = strings.TrimSpace(cfg.VisionBackupModel1)
	cfg.VisionBackupModel2 = strings.TrimSpace(cfg.VisionBackupModel2)
	cfg.VisionBackupModel3 = strings.TrimSpace(cfg.VisionBackupModel3)
	cfg.OnVisionFailure = strings.ToLower(strings.TrimSpace(cfg.OnVisionFailure))
	if cfg.OnVisionFailure == "" {
		cfg.OnVisionFailure = def.OnVisionFailure
	}
	if cfg.OnVisionFailure != "error" && cfg.OnVisionFailure != "text_only" {
		return cfg, fmt.Errorf("on_vision_failure must be error or text_only")
	}
	clean := make([]visionModel, 0, len(cfg.VisionModels))
	for _, item := range cfg.VisionModels {
		item.Model = strings.TrimSpace(item.Model)
		if item.Model == "" {
			continue
		}
		if item.ContextLimit <= 0 {
			item.ContextLimit = 262144
		}
		clean = append(clean, item)
	}
	if cfg.VisionPrimaryModel != "" || cfg.VisionBackupModel1 != "" || cfg.VisionBackupModel2 != "" || cfg.VisionBackupModel3 != "" {
		clean = clean[:0]
		for _, model := range []string{cfg.VisionPrimaryModel, cfg.VisionBackupModel1, cfg.VisionBackupModel2, cfg.VisionBackupModel3} {
			if model != "" {
				clean = append(clean, visionModel{Model: model, ContextLimit: cfg.VisionContextLimit})
			}
		}
	}
	cfg.VisionModels = clean
	return cfg, nil
}

type cacheRecord struct {
	Value   string
	Expires time.Time
}
type memoCache struct {
	mu     sync.Mutex
	values map[string]cacheRecord
	limit  int
}

func newMemoCache(limit int) *memoCache {
	return &memoCache{values: map[string]cacheRecord{}, limit: limit}
}
func (m *memoCache) get(k string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[k]
	if !ok || time.Now().After(v.Expires) {
		delete(m.values, k)
		return "", false
	}
	return v.Value, true
}
func (m *memoCache) set(k, value string, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.values) >= m.limit {
		for old := range m.values {
			delete(m.values, old)
			break
		}
	}
	m.values[k] = cacheRecord{Value: value, Expires: time.Now().Add(ttl)}
}

type visualAsset struct {
	URL  string
	Path []string
}

// transformOpenAIRequest replaces every image part with extracted visual memory.
// It supports Chat Completions image_url parts and Responses input_image parts.
// An over-limit request is rejected before any vision call, never partially
// transformed and never forwarded with an unredacted image.
func transformOpenAIRequest(raw []byte, cfg runtimeConfig, describe func(visualAsset, string) (string, error)) ([]byte, int, error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, 0, fmt.Errorf("invalid OpenAI request JSON: %w", err)
	}
	contextText := trimToTokens(extractText(root), cfg.VisionInputTokenBudget)
	assets := collectVisualAssets(root)
	if len(assets) == 0 {
		return raw, 0, nil
	}
	if err := enforceVisualImageLimit(cfg, assets); err != nil {
		return nil, len(assets), err
	}
	for _, asset := range assets {
		if !allowedAsset(asset.URL, cfg) {
			return nil, len(assets), fmt.Errorf("image is blocked by plugin safety limits")
		}
		description, err := describe(asset, contextText)
		if err != nil {
			if cfg.OnVisionFailure == "text_only" {
				description = "Visual input could not be analyzed; continue using only the text in this request."
			} else {
				return nil, 0, err
			}
		}
		replaceAsset(root, asset.Path, description)
	}
	result, err := json.Marshal(root)
	return result, len(assets), err
}

// enforceVisualImageLimit limits only uncached, distinct images which would
// require a vision-model call. Cached historical images still get replaced by
// their visual memory, regardless of how many occur in a replayed history.
func enforceVisualImageLimit(cfg runtimeConfig, assets []visualAsset) error {
	seen := make(map[string]struct{}, len(assets))
	uncached := 0
	for _, asset := range assets {
		key := visualCacheKey(cfg, asset)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if _, cached := cfg.cache.get(key); cached {
			continue
		}
		uncached++
		if uncached > cfg.MaxImagesPerRequest {
			return fmt.Errorf("request contains %d uncached images; maximum is %d. Request was blocked so no unconverted image can reach the primary text model", uncached, cfg.MaxImagesPerRequest)
		}
	}
	return nil
}

func allowedAsset(raw string, cfg runtimeConfig) bool {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "data:") {
		return len(raw) <= cfg.MaxImageDataBytes*2
	}
	return cfg.AllowRemoteImageURLs && (strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://"))
}

func collectVisualAssets(root any) []visualAsset {
	var out []visualAsset
	var walk func(any, []string)
	walk = func(v any, path []string) {
		switch x := v.(type) {
		case map[string]any:
			typ, _ := x["type"].(string)
			if typ == "image_url" || typ == "input_image" {
				if u := imageURL(x); u != "" {
					out = append(out, visualAsset{URL: u, Path: append([]string(nil), path...)})
					return
				}
			}
			for k, child := range x {
				walk(child, append(path, k))
			}
		case []any:
			for i, child := range x {
				walk(child, append(path, fmt.Sprintf("#%d", i)))
			}
		}
	}
	walk(root, nil)
	return out
}
func imageURL(item map[string]any) string {
	if raw, ok := item["image_url"].(string); ok {
		return raw
	}
	if raw, ok := item["url"].(string); ok {
		return raw
	}
	if obj, ok := item["image_url"].(map[string]any); ok {
		if raw, ok := obj["url"].(string); ok {
			return raw
		}
	}
	return ""
}
func replaceAsset(root any, path []string, description string) {
	if len(path) == 0 {
		return
	}
	var current any = root
	for i, step := range path {
		last := i == len(path)-1
		if strings.HasPrefix(step, "#") {
			var index int
			_, _ = fmt.Sscanf(step, "#%d", &index)
			array, ok := current.([]any)
			if !ok || index < 0 || index >= len(array) {
				return
			}
			if last {
				array[index] = map[string]any{"type": "text", "text": "[Visual memory]\n" + description}
				return
			}
			current = array[index]
		} else {
			obj, ok := current.(map[string]any)
			if !ok {
				return
			}
			if last {
				obj[step] = map[string]any{"type": "text", "text": "[Visual memory]\n" + description}
				return
			}
			current = obj[step]
		}
	}
}

func extractText(root any) string {
	var parts []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			parts = append(parts, x)
		case map[string]any:
			for k, child := range x {
				if k != "image_url" && k != "url" {
					walk(child)
				}
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(root)
	return strings.Join(parts, "\n")
}
func trimToTokens(s string, tokens int) string {
	if tokens <= 0 || len(s) <= tokens*4 {
		return s
	}
	return s[len(s)-tokens*4:]
}
func visualCacheKey(cfg runtimeConfig, asset visualAsset) string {
	sum := sha256.Sum256([]byte(cfg.VisionPrompt + "\x00" + asset.URL))
	return hex.EncodeToString(sum[:])
}
func estimateTokens(text string) int { return (len(text) + 3) / 4 }

func makeVisionRequest(model, prompt, contextText, imageURL string, maxOutput int) []byte {
	body := map[string]any{"model": model, "temperature": 0, "max_tokens": maxOutput, "messages": []any{
		map[string]any{"role": "system", "content": prompt},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Relevant request context:\n" + contextText}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}}}},
	}}
	raw, _ := json.Marshal(body)
	return raw
}

func extractVisionText(raw []byte) string {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return ""
	}
	if choices, ok := root["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				return strings.TrimSpace(contentText(msg["content"]))
			}
		}
	}
	return strings.TrimSpace(contentText(root["output_text"]))
}
func contentText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, item := range x {
			if obj, ok := item.(map[string]any); ok {
				if text, ok := obj["text"].(string); ok {
					parts = append(parts, text)
				}
				if text, ok := obj["content"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
