package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	cpaConfigPath            = "/CLIProxyAPI/config.yaml"
	modelCatalogCacheTTL     = 15 * time.Second
	modelCatalogTimeout      = 2 * time.Second
	defaultCPAManagementPort = 8317
)

var modelCatalogCache struct {
	sync.Mutex
	models    []string
	expiresAt time.Time
}

type dashboardConfig struct {
	Enabled                        bool     `json:"enabled"`
	ComboModel                     string   `json:"combo_model"`
	ComboAliases                   []string `json:"combo_aliases"`
	PrimaryModel                   string   `json:"primary_model"`
	PrimaryContextTokens           int      `json:"primary_context_tokens"`
	PrimaryContextBudgetTokens     int      `json:"primary_context_budget_tokens"`
	TextFallbackModels             []string `json:"text_fallback_models"`
	VisionPrimaryModel             string   `json:"vision_primary_model"`
	VisionBackupModel1             string   `json:"vision_backup_model_1"`
	VisionBackupModel2             string   `json:"vision_backup_model_2"`
	VisionBackupModel3             string   `json:"vision_backup_model_3"`
	VisionContextLimit             int      `json:"vision_context_limit"`
	VisionInputTokenBudget         int      `json:"vision_input_token_budget"`
	VisionOutputTokens             int      `json:"vision_output_tokens"`
	VisionTimeoutSeconds           int      `json:"vision_timeout_seconds"`
	CacheTTLSeconds                int      `json:"cache_ttl_seconds"`
	CacheMaxEntries                int      `json:"cache_max_entries"`
	EventLogMaxEntries             int      `json:"event_log_max_entries"`
	OnVisionFailure                string   `json:"on_vision_failure"`
	StrictVisionFailure            bool     `json:"strict_vision_failure"`
	MaxImagesPerRequest            int      `json:"max_images_per_request"`
	MaxConcurrentExtractions       int      `json:"max_concurrent_extractions"`
	MaxImageDataBytes              int      `json:"max_image_data_bytes"`
	AllowRemoteImageURLs           bool     `json:"allow_remote_image_urls"`
	HistoryAttachmentMode          string   `json:"history_attachment_mode"`
	HistoryAttachmentCompactChars  int      `json:"history_attachment_compact_chars"`
	HistoryRestoreMaxAttachments   int      `json:"history_attachment_restore_max_attachments"`
	AutoCompressionEnabled         bool     `json:"auto_compression_enabled"`
	AutoCompressionThresholdTokens int      `json:"auto_compression_threshold_tokens"`
	AutoCompressionTargetTokens    int      `json:"auto_compression_target_tokens"`
	AutoCompressionKeepRecentTurns int      `json:"auto_compression_keep_recent_turns"`
	AutoCompressionModel           string   `json:"auto_compression_model"`
}

func dashboardConfigFrom(cfg runtimeConfig) dashboardConfig {
	return dashboardConfig{
		Enabled: cfg.Enabled, ComboModel: cfg.ComboModel, ComboAliases: cfg.ComboAliases,
		PrimaryModel: cfg.PrimaryModel, PrimaryContextTokens: cfg.PrimaryContextTokens,
		PrimaryContextBudgetTokens: cfg.PrimaryContextBudgetTokens, TextFallbackModels: cfg.TextFallbackModels,
		VisionPrimaryModel: cfg.VisionPrimaryModel, VisionBackupModel1: cfg.VisionBackupModel1,
		VisionBackupModel2: cfg.VisionBackupModel2, VisionBackupModel3: cfg.VisionBackupModel3,
		VisionContextLimit: cfg.VisionContextLimit, VisionInputTokenBudget: cfg.VisionInputTokenBudget,
		VisionOutputTokens: cfg.VisionOutputTokens, VisionTimeoutSeconds: cfg.VisionTimeoutSeconds,
		CacheTTLSeconds: cfg.CacheTTLSeconds, CacheMaxEntries: cfg.CacheMaxEntries,
		EventLogMaxEntries: cfg.EventLogMaxEntries, OnVisionFailure: cfg.OnVisionFailure,
		StrictVisionFailure: cfg.StrictVisionFailure, MaxImagesPerRequest: cfg.MaxImagesPerRequest,
		MaxConcurrentExtractions: cfg.MaxConcurrentExtractions, MaxImageDataBytes: cfg.MaxImageDataBytes,
		AllowRemoteImageURLs: cfg.AllowRemoteImageURLs, HistoryAttachmentMode: cfg.HistoryAttachmentMode,
		HistoryAttachmentCompactChars:  cfg.HistoryAttachmentCompactChars,
		HistoryRestoreMaxAttachments:   cfg.HistoryRestoreMaxAttachments,
		AutoCompressionEnabled:         cfg.AutoCompressionEnabled,
		AutoCompressionThresholdTokens: cfg.AutoCompressionThresholdTokens,
		AutoCompressionTargetTokens:    cfg.AutoCompressionTargetTokens,
		AutoCompressionKeepRecentTurns: cfg.AutoCompressionKeepRecentTurns,
		AutoCompressionModel:           cfg.AutoCompressionModel,
	}
}

func managementJSONResponse(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{StatusCode: 200, Headers: map[string][]string{"content-type": {"application/json; charset=utf-8"}, "cache-control": {"no-store"}}, Body: body})
}

func currentModelCatalog(_ runtimeConfig) []string {
	raw, err := os.ReadFile(cpaConfigPath)
	if err != nil {
		return nil
	}
	return runtimeCPAModels(raw)
}

func runtimeCPAModels(configYAML []byte) []string {
	now := time.Now()
	modelCatalogCache.Lock()
	if now.Before(modelCatalogCache.expiresAt) {
		cached := append([]string(nil), modelCatalogCache.models...)
		modelCatalogCache.Unlock()
		return cached
	}
	modelCatalogCache.Unlock()
	port, apiKey := cpaLocalAPISettings(configYAML)
	if apiKey == "" {
		return nil
	}
	client := &http.Client{Timeout: modelCatalogTimeout}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/models", port), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	set := map[string]struct{}{}
	for _, item := range body.Data {
		if id := strings.TrimSpace(item.ID); id != "" && id != "glm-5.2-vision-combo" && id != "glm-vision-bridge" {
			set[id] = struct{}{}
		}
	}
	models := make([]string, 0, len(set))
	for model := range set {
		models = append(models, model)
	}
	sort.Strings(models)
	modelCatalogCache.Lock()
	modelCatalogCache.models = append([]string(nil), models...)
	modelCatalogCache.expiresAt = now.Add(modelCatalogCacheTTL)
	modelCatalogCache.Unlock()
	return models
}

func cpaLocalAPISettings(configYAML []byte) (int, string) {
	var root map[string]any
	if yaml.Unmarshal(configYAML, &root) != nil {
		return defaultCPAManagementPort, ""
	}
	port := defaultCPAManagementPort
	switch value := root["port"].(type) {
	case int:
		if value > 0 && value < 65536 {
			port = value
		}
	case int64:
		if value > 0 && value < 65536 {
			port = int(value)
		}
	case float64:
		if value > 0 && value < 65536 {
			port = int(value)
		}
	}
	keys, _ := root["api-keys"].([]any)
	for _, value := range keys {
		if key, ok := value.(string); ok && strings.TrimSpace(key) != "" {
			return port, strings.TrimSpace(key)
		}
	}
	return port, ""
}

func managementHTML(cfg runtimeConfig) string {
	events := cfg.events.snapshot()
	sortEventsForDisplay(events)
	payload, _ := json.Marshal(struct {
		Config dashboardConfig `json:"config"`
		Models []string        `json:"models"`
		Events []comboEvent    `json:"events"`
	}{dashboardConfigFrom(cfg), currentModelCatalog(cfg), events})
	jsonForScript := strings.NewReplacer("<", "\\u003c", ">", "\\u003e", "&", "\\u0026").Replace(string(payload))
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>GLM Vision Bridge</title><style>
:root{--bg:#f5f7fa;--paper:#fff;--ink:#172033;--muted:#647084;--line:#dce2ea;--line2:#edf0f4;--blue:#245fc7;--blue2:#eaf2ff;--green:#14785c;--green2:#e9f7f1;--amber:#9a620b;--amber2:#fff7e2;--red:#b34444;--red2:#fff0f0;--shadow:0 12px 34px rgba(26,39,64,.07)}*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}button,input,select{font:inherit}main{max-width:1440px;margin:auto;padding:26px}.top{display:flex;justify-content:space-between;gap:24px;align-items:flex-start;margin-bottom:18px}.kicker{color:var(--blue);font-size:12px;font-weight:750;letter-spacing:.13em}.top h1{font-size:30px;letter-spacing:-.04em;margin:5px 0 8px}.top p{margin:0;max-width:760px;line-height:1.7;color:var(--muted)}.badge{display:inline-flex;align-items:center;gap:7px;border:1px solid #bcd3f7;background:var(--blue2);color:#174d9f;padding:7px 10px;border-radius:8px;font-size:12px;white-space:nowrap}.dot{width:7px;height:7px;border-radius:50%;background:#26a575}.stats{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:10px;margin-bottom:18px}.stat{background:var(--paper);border:1px solid var(--line);padding:13px 15px;min-height:72px}.stat b{display:block;font-size:16px;line-height:1.35;word-break:break-all}.stat span{display:block;margin-top:5px;color:var(--muted);font-size:12px}.tabs{display:flex;gap:4px;border-bottom:1px solid var(--line);margin-bottom:18px}.tab{border:0;background:none;color:var(--muted);padding:11px 16px;cursor:pointer}.tab.active{color:var(--blue);font-weight:750;border-bottom:2px solid var(--blue)}.view{display:none}.view.active{display:block}.panel{background:var(--paper);border:1px solid var(--line);box-shadow:var(--shadow)}.panel-h{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;padding:18px 20px;border-bottom:1px solid var(--line)}.panel-h h2,.section-title h3{margin:0;font-size:16px}.panel-h p,.section-title p{margin:5px 0 0;color:var(--muted);font-size:12px;line-height:1.6}.flow-wrap{padding:22px}.flow{display:grid;grid-template-columns:minmax(130px,.8fr) 28px minmax(150px,1fr) 28px minmax(250px,1.7fr) 28px minmax(155px,1fr);align-items:stretch}.arrow{display:grid;place-items:center;color:#8da0ba;font-size:20px}.node{border:1px solid var(--line);background:#fff;padding:15px;min-height:112px;position:relative}.node.blue{border-color:#b9d1f6;background:#f8fbff}.node.green{border-color:#b8dece;background:#f8fdfa}.node h4{margin:0 0 7px;font-size:13px}.node p{margin:0;color:var(--muted);font-size:12px;line-height:1.55}.node .model{display:block;margin-top:10px;font:600 12px ui-monospace,SFMono-Regular,Menlo,monospace;color:#1e4f9f;word-break:break-all}.branch{display:grid;gap:8px}.mini-node{border:1px solid var(--line);background:#fff;padding:10px 12px;min-height:51px}.mini-node strong{display:block;font-size:12px}.mini-node span{display:block;color:var(--muted);font-size:11px;margin-top:3px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.flow-notes{display:grid;grid-template-columns:repeat(3,1fr);gap:10px;margin-top:16px}.note{border-top:2px solid #b9c9dd;background:#f7f9fc;padding:11px 12px;color:#49566a;font-size:12px;line-height:1.55}.note strong{color:var(--ink)}.config-layout{display:grid;grid-template-columns:minmax(0,1fr) 330px;gap:18px}.config-sections{display:grid;gap:14px}.config-section{background:var(--paper);border:1px solid var(--line)}.section-title{padding:16px 18px;border-bottom:1px solid var(--line2)}.fields{display:grid;grid-template-columns:repeat(2,minmax(0,1fr))}.field{padding:14px 16px;border-right:1px solid var(--line2);border-bottom:1px solid var(--line2)}.field:nth-child(2n){border-right:0}.field.full{grid-column:1/-1;border-right:0}.field label{display:block;font-weight:700;font-size:13px;margin-bottom:7px}.field small{display:block;margin-top:6px;color:var(--muted);font-size:11px;line-height:1.5}.field input,.field select{width:100%;height:38px;border:1px solid #bcc8d8;background:#fff;color:var(--ink);padding:0 10px;border-radius:5px;outline:none}.field input:focus,.field select:focus{border-color:#6994db;box-shadow:0 0 0 3px rgba(36,95,199,.09)}.switch-field{display:flex;justify-content:space-between;gap:18px;align-items:flex-start}.switch{width:42px!important;height:23px!important;accent-color:var(--blue);flex:0 0 auto}.side{position:sticky;top:16px;align-self:start;display:grid;gap:14px}.side-card{background:var(--paper);border:1px solid var(--line);padding:16px}.side-card h3{font-size:14px;margin:0 0 8px}.side-card p{font-size:12px;line-height:1.65;color:var(--muted);margin:0}.mini-flow{display:grid;gap:7px;margin-top:12px}.mini-flow div{border:1px solid var(--line);padding:8px 9px;font-size:12px;background:#fafbfd}.mini-flow .active{border-color:#aac8f6;background:var(--blue2);color:#174d9f}.save{display:grid;gap:9px}.button{height:38px;border:1px solid #b9c7da;background:#fff;color:#264c88;padding:0 13px;border-radius:5px;cursor:pointer}.button.primary{background:var(--blue);border-color:var(--blue);color:#fff;font-weight:700}.save-note{font-size:11px;color:var(--muted);line-height:1.5}.events-layout{display:grid;grid-template-columns:320px minmax(0,1fr);gap:16px}.event-list{max-height:680px;overflow:auto}.event-item{width:100%;text-align:left;border:0;border-bottom:1px solid var(--line2);background:#fff;padding:13px 15px;cursor:pointer}.event-item:hover,.event-item.active{background:#f3f7fd}.event-title{display:flex;justify-content:space-between;gap:10px;font-weight:700}.event-meta{font-size:11px;color:var(--muted);margin-top:5px;line-height:1.5}.status{font-size:11px;padding:2px 6px;white-space:nowrap}.status.完成{background:var(--green2);color:var(--green)}.status.进行中{background:var(--blue2);color:var(--blue)}.status.失败{background:var(--red2);color:var(--red)}.detail{padding:18px}.empty{padding:54px 20px;text-align:center;color:var(--muted)}.stage{display:grid;grid-template-columns:150px minmax(0,1fr) 92px;gap:12px;padding:13px 0;border-bottom:1px solid var(--line2)}.stage-name{font-weight:700;margin-bottom:5px}.stage-detail{white-space:pre-wrap;word-break:break-word;line-height:1.55;color:#344056}.stage-meta{text-align:right;color:var(--muted);font-size:11px}.tag{font:11px ui-monospace,SFMono-Regular,Menlo,monospace;background:#f0f3f7;padding:3px 6px;word-break:break-all}.alert{padding:11px 12px;background:var(--amber2);color:#77500f;border:1px solid #efd8a2;font-size:12px;line-height:1.6;margin-bottom:14px}.hidden{display:none}@media(max-width:1050px){.flow{grid-template-columns:1fr}.arrow{height:30px;transform:rotate(90deg)}.config-layout{grid-template-columns:1fr}.side{position:static}.stats{grid-template-columns:repeat(2,1fr)}}@media(max-width:720px){main{padding:16px}.top{display:block}.badge{margin-top:14px}.stats,.fields,.flow-notes{grid-template-columns:1fr}.field,.field:nth-child(2n){border-right:0}.events-layout{grid-template-columns:1fr}.event-list{max-height:280px}.stage{grid-template-columns:1fr}.stage-meta{text-align:left}}
</style></head><body><main><header class="top"><div><div class="kicker">CPA 原生插件 · 视觉桥接 v0.3</div><h1>GLM Vision Bridge</h1><p>视觉模型只负责把图片转成不可信的事实文本；主任务仍由首选文本模型完成。历史图片按需恢复，长对话超过阈值后自动压缩。</p></div><div class="badge"><span class="dot"></span><span id="enabledBadge">插件已启用</span></div></header><section class="stats"><div class="stat"><b id="comboStat">—</b><span>对外模型</span></div><div class="stat"><b id="primaryStat">—</b><span>首选文本模型</span></div><div class="stat"><b id="visionStat">—</b><span>首选视觉模型</span></div><div class="stat"><b id="eventStat">0</b><span>当前内存事件</span></div></section><nav class="tabs"><button class="tab active" data-view="overview">路由预览</button><button class="tab" data-view="settings">功能配置</button><button class="tab" data-view="events">请求记录</button></nav>
<section id="overview" class="view active panel"><div class="panel-h"><div><h2>当前 Combo 的实际路由</h2><p>配置修改时，下方模型名称和策略会同步预览；保存后才影响真实流量。</p></div><span class="tag" id="previewCombo">—</span></div><div class="flow-wrap"><div class="flow"><div class="node blue"><h4>1. Agent 请求</h4><p>统一接收纯文本或图片请求，不改变客户端的会话模型。</p><span class="model" id="flowCombo">—</span></div><div class="arrow">→</div><div class="node"><h4>2. 附件与上下文判断</h4><p>当前图片完整识别；旧图片默认只保留短摘要，明确引用时恢复详情。</p><span class="model" id="flowHistory">按需恢复</span></div><div class="arrow">→</div><div class="branch" id="visionBranch"></div><div class="arrow">→</div><div class="node green"><h4>4. 文本模型完成任务</h4><p>识别文本以 untrusted 形式注入；主模型失败前不会改变会话路由。</p><span class="model" id="flowPrimary">—</span></div></div><div class="flow-notes"><div class="note"><strong>纯文本轮：</strong>完全跳过视觉链，只有达到压缩阈值时才多一次摘要调用。</div><div class="note"><strong>图片轮：</strong>同图命中缓存不重复识别；同一时刻的重复请求会合并。</div><div class="note"><strong>失败策略：</strong>单个视觉模型失败自动尝试下一个；全部失败时按严格策略返回错误。</div></div></div></section>
<section id="settings" class="view"><form id="configForm"><div class="config-layout"><div class="config-sections"><div class="config-section"><div class="section-title"><h3>1. 对外入口与文本模型链</h3><p>已有 Agent 可继续使用旧模型名；别名可以提供更直观的新入口。</p></div><div class="fields"><div class="field"><label>主要对外模型名</label><input name="combo_model"><small>建议保留 glm-5.2-vision-combo，避免已有调用失效。</small></div><div class="field"><label>对外别名（逗号分隔）</label><input name="combo_aliases"><small>例如 glm-vision-bridge；这些名称暴露相同能力。</small></div><div class="field"><label>首选文本模型</label><select name="primary_model" data-model></select><small>无图和识图后的最终回答都首先交给它。</small></div><div class="field"><label>文本备用模型 1</label><select name="text_fallback_0" data-model></select><small>首选模型在输出任何内容前失败时使用。</small></div><div class="field"><label>文本备用模型 2</label><select name="text_fallback_1" data-model></select><small>可留空；不会调用视觉模型来生成最终答案。</small></div><div class="field"><label>文本备用模型 3</label><select name="text_fallback_2" data-model></select><small>流式回答已输出后不会切换，避免混合两份答案。</small></div></div></div>
<div class="config-section"><div class="section-title"><h3>2. 视觉候选链</h3><p>视觉模型按顺序逐个尝试，所有识别子请求强制 low 思考，并清除外层 system、tools 和 reasoning 配置。</p></div><div class="fields"><div class="field"><label>首选视觉模型</label><select name="vision_primary_model" data-model></select><small>图片轮首先调用；建议选择延迟较低、截图 OCR 稳定的模型。</small></div><div class="field"><label>备用视觉模型 1</label><select name="vision_backup_model_1" data-model></select><small>首选报错、无有效内容或由 CPA Host 判定失败后调用。</small></div><div class="field"><label>备用视觉模型 2</label><select name="vision_backup_model_2" data-model></select><small>可留空。</small></div><div class="field"><label>备用视觉模型 3</label><select name="vision_backup_model_3" data-model></select><small>可留空，最多四级视觉链。</small></div><div class="field"><label>视觉模型上下文上限</label><input type="number" name="vision_context_limit"><small>当前四个候选共享此值；256K 建议填写 262144 或实际可用上限。</small></div><div class="field"><label>单模型软延迟预算（秒）</label><input type="number" name="vision_timeout_seconds"><small>用于观测慢调用。CPA Host 暂不支持取消；插件会接收成功的迟到结果，避免后台遗留请求与重复计费。</small></div><div class="field"><label>附近文字预算（tokens）</label><input type="number" name="vision_input_token_budget"><small>只带当前用户问题附近文字，不再把完整 1M 会话发送给 256K 视觉模型。</small></div><div class="field"><label>识别输出上限（tokens）</label><input type="number" name="vision_output_tokens"><small>复杂表格可提高；普通截图通常 1K–4K 足够。</small></div></div></div>
<div class="config-section"><div class="section-title"><h3>3. 主上下文、历史图片与自动压缩</h3><p>按需恢复减少非图片续问中的上下文；自动压缩仅在接近阈值时触发。</p></div><div class="fields"><div class="field"><label>主模型上下文上限</label><input type="number" name="primary_context_tokens"><small>GLM 可接收的理论上限，例如 1048576。</small></div><div class="field"><label>主模型工作预算</label><input type="number" name="primary_context_budget_tokens"><small>必须小于上下文上限，预留输出和路由安全空间。</small></div><div class="field"><label>历史图片策略</label><select name="history_attachment_mode"><option value="onDemand">按需恢复（推荐）</option><option value="retain">每轮完整保留</option></select><small>按需模式在明确提到图片、截图、附件等词时恢复详情。</small></div><div class="field"><label>历史图片短摘要字符数</label><input type="number" name="history_attachment_compact_chars"><small>无关轮只注入这段短摘要，不保存或重传原图。</small></div><div class="field"><label>单轮最多恢复旧图片数</label><input type="number" name="history_attachment_restore_max_attachments"><small>只恢复最近的旧图片，并受总图片数限制。</small></div><div class="field"><label>并发识图数</label><input type="number" name="max_concurrent_extractions"><small>多图并发；单张图片内部仍按候选顺序回退。</small></div><div class="field switch-field full"><div><label>启用自动压缩长对话</label><small>达到阈值后把较早历史压缩为 untrusted 摘要，保留最近轮次原文。</small></div><input class="switch" type="checkbox" name="auto_compression_enabled"></div><div class="field"><label>压缩触发阈值（tokens）</label><input type="number" name="auto_compression_threshold_tokens"><small>建议低于工作预算约 15%–25%。</small></div><div class="field"><label>摘要目标（tokens）</label><input type="number" name="auto_compression_target_tokens"><small>越小越省上下文；越大保留更多历史细节。</small></div><div class="field"><label>保留最近轮数</label><input type="number" name="auto_compression_keep_recent_turns"><small>这些最近消息保持原文，不进入摘要。</small></div><div class="field"><label>压缩模型</label><select name="auto_compression_model" data-model></select><small>留空使用首选文本模型；失败后尝试文本备用链。</small></div></div></div>
<div class="config-section"><div class="section-title"><h3>4. 缓存、容量与失败保护</h3><p>持久缓存只保存识别后的文本和摘要，不长期保存原图；data URL 按真实解码字节校验。</p></div><div class="fields"><div class="field"><label>缓存时长（秒）</label><input type="number" name="cache_ttl_seconds"><small>默认 72 小时为 259200 秒；重启插件后仍可命中。</small></div><div class="field"><label>缓存最大条数</label><input type="number" name="cache_max_entries"><small>使用 LRU 淘汰，写入在后台合并，不增加回答延迟。</small></div><div class="field"><label>当前轮最大图片数</label><input type="number" name="max_images_per_request"><small>历史短摘要不计入；恢复详情时仍受总容量限制。</small></div><div class="field"><label>单张内嵌图片上限（字节）</label><input type="number" name="max_image_data_bytes"><small>校验 base64 解码后的真实字节，不再按字符串长度猜测。</small></div><div class="field switch-field"><div><label>允许远程图片 URL</label><small>远程 URL 内容可能变化，因此不会进入持久缓存。</small></div><input class="switch" type="checkbox" name="allow_remote_image_urls"></div><div class="field switch-field"><div><label>严格处理视觉失败</label><small>所有视觉模型失败时直接报错，避免文本模型猜图。</small></div><input class="switch" type="checkbox" name="strict_vision_failure"></div><div class="field"><label>事件日志保留条数</label><input type="number" name="event_log_max_entries"><small>仅保留阶段、模型、耗时与截断后的描述，不保存原图。</small></div></div></div></div>
<aside class="side"><div class="side-card"><h3>保存前流程预览</h3><p>下列顺序随表单实时更新。所有配置保存后由 CPA 自动重新加载插件。</p><div class="mini-flow" id="sideFlow"></div></div><div class="side-card save"><label><strong>CPA Manager Plus 管理密钥</strong></label><input id="managerKey" type="password" autocomplete="off" placeholder="仅用于本次保存"><button class="button primary" type="submit">保存并重新加载插件</button><span class="save-note" id="saveNote">保存前不会影响当前生产流量。密钥不会写入插件配置或浏览器存储。</span></div></aside></div></form></section>
<section id="events" class="view"><div class="events-layout"><aside class="panel"><div class="panel-h"><div><h2>最近请求</h2><p>完整统计从接收请求到输出流关闭。</p></div><button class="button" id="refresh">刷新</button></div><div id="eventList" class="event-list"></div></aside><section class="panel"><div class="panel-h"><div><h2>实际执行过程</h2><p>可核对使用了哪些视觉/文本模型及每一步耗时。</p></div><span id="selectedId" class="tag">未选择</span></div><div id="eventDetail" class="detail"></div></section></div></section></main>
<script>const DATA=` + jsonForScript + `;const $=s=>document.querySelector(s);const $$=s=>[...document.querySelectorAll(s)];const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));const C=DATA.config,M=[...new Set(DATA.models||[])].sort(),E=DATA.events||[];let selected=E[0]?.id||'';function modelOptions(value,allowEmpty=true){let a=allowEmpty?'<option value="">不选择</option>':'';if(value&&!M.includes(value))a+='<option selected value="'+esc(value)+'">'+esc(value)+'（当前配置）</option>';return a+M.map(x=>'<option '+(x===value?'selected':'')+' value="'+esc(x)+'">'+esc(x)+'</option>').join('')}function setValue(name,value){const e=document.querySelector('[name="'+name+'"]');if(!e)return;if(e.type==='checkbox')e.checked=!!value;else e.value=value??''}function get(name){const e=document.querySelector('[name="'+name+'"]');return e?(e.type==='checkbox'?e.checked:e.value):''}function init(){setValue('combo_model',C.combo_model);setValue('combo_aliases',(C.combo_aliases||[]).join(', '));setValue('primary_context_tokens',C.primary_context_tokens);setValue('primary_context_budget_tokens',C.primary_context_budget_tokens);setValue('vision_context_limit',C.vision_context_limit);setValue('vision_timeout_seconds',C.vision_timeout_seconds);setValue('vision_input_token_budget',C.vision_input_token_budget);setValue('vision_output_tokens',C.vision_output_tokens);setValue('history_attachment_mode',C.history_attachment_mode);setValue('history_attachment_compact_chars',C.history_attachment_compact_chars);setValue('history_attachment_restore_max_attachments',C.history_attachment_restore_max_attachments);setValue('max_concurrent_extractions',C.max_concurrent_extractions);setValue('auto_compression_enabled',C.auto_compression_enabled);setValue('auto_compression_threshold_tokens',C.auto_compression_threshold_tokens);setValue('auto_compression_target_tokens',C.auto_compression_target_tokens);setValue('auto_compression_keep_recent_turns',C.auto_compression_keep_recent_turns);setValue('cache_ttl_seconds',C.cache_ttl_seconds);setValue('cache_max_entries',C.cache_max_entries);setValue('max_images_per_request',C.max_images_per_request);setValue('max_image_data_bytes',C.max_image_data_bytes);setValue('allow_remote_image_urls',C.allow_remote_image_urls);setValue('strict_vision_failure',C.strict_vision_failure);setValue('event_log_max_entries',C.event_log_max_entries);const modelValues={primary_model:C.primary_model,text_fallback_0:C.text_fallback_models?.[0]||'',text_fallback_1:C.text_fallback_models?.[1]||'',text_fallback_2:C.text_fallback_models?.[2]||'',vision_primary_model:C.vision_primary_model,vision_backup_model_1:C.vision_backup_model_1,vision_backup_model_2:C.vision_backup_model_2,vision_backup_model_3:C.vision_backup_model_3,auto_compression_model:C.auto_compression_model};Object.entries(modelValues).forEach(([n,v])=>{const e=document.querySelector('[name="'+n+'"]');e.innerHTML=modelOptions(v,n!=='primary_model'&&n!=='vision_primary_model')});$('#enabledBadge').textContent=C.enabled?'插件已启用':'插件已停用';$('.dot').style.background=C.enabled?'#26a575':'#c34b4b';$('#eventStat').textContent=E.length;renderPreview();renderEvents()}function renderPreview(){const combo=get('combo_model')||'未设置',primary=get('primary_model')||'未设置',visions=['vision_primary_model','vision_backup_model_1','vision_backup_model_2','vision_backup_model_3'].map(get).filter(Boolean),texts=['text_fallback_0','text_fallback_1','text_fallback_2'].map(get).filter(Boolean);$('#comboStat').textContent=combo;$('#primaryStat').textContent=primary;$('#visionStat').textContent=visions[0]||'未设置';$('#previewCombo').textContent=combo;$('#flowCombo').textContent=combo;$('#flowPrimary').textContent=primary+(texts.length?' → '+texts.join(' → '):'');$('#flowHistory').textContent=get('history_attachment_mode')==='retain'?'每轮完整保留':'按需恢复 · 无关轮短摘要';$('#visionBranch').innerHTML=(visions.length?visions:['未设置视觉模型']).map((m,i)=>'<div class="mini-node"><strong>'+(i===0?'3. 首选视觉模型':'备用视觉模型 '+i)+'</strong><span>'+esc(m)+(i===0?' · 强制 low':' · 前序失败后调用')+'</span></div>').join('');$('#sideFlow').innerHTML='<div class="active">Agent → '+esc(combo)+'</div><div>当前图片：'+esc(visions.join(' → ')||'未设置')+'</div><div>视觉文本：untrusted 包装</div><div>最终回答：'+esc(primary+(texts.length?' → '+texts.join(' → '):''))+'</div><div>历史：'+(get('history_attachment_mode')==='retain'?'完整保留':'按需恢复')+'；压缩：'+(get('auto_compression_enabled')?'开启':'关闭')+'</div>'}function renderEvents(){const list=$('#eventList');if(!E.length){list.innerHTML='<div class="empty">尚无请求。调用 <span class="tag">'+esc(C.combo_model)+'</span> 后，这里会显示真实路由。</div>';$('#eventDetail').innerHTML='<div class="empty">记录将展示缓存命中、视觉回退、自动压缩、文本回退和完整耗时。</div>';return}list.innerHTML=E.map(e=>'<button class="event-item '+(e.id===selected?'active':'')+'" data-id="'+esc(e.id)+'"><div class="event-title"><span>'+esc(e.id)+'</span><span class="status '+esc(e.status)+'">'+esc(e.status)+'</span></div><div class="event-meta">'+(e.image_count?'图片 '+e.image_count+' 张':'纯文本')+' · '+(e.stream?'流式':'非流式')+' · '+new Date(e.started_at).toLocaleTimeString()+'</div></button>').join('');$$('[data-id]').forEach(b=>b.onclick=()=>{selected=b.dataset.id;renderEvents()});const e=E.find(x=>x.id===selected)||E[0];$('#selectedId').textContent=e.id;$('#eventDetail').innerHTML=(e.error?'<div class="alert">本次请求失败：'+esc(e.error)+'</div>':'')+e.stages.map(s=>'<div class="stage"><div><div class="stage-name">'+esc(s.name)+'</div><span class="status '+esc(s.status)+'">'+esc(s.status)+'</span></div><div class="stage-detail">'+esc(s.detail||'—')+'</div><div class="stage-meta">'+(s.model?'<span class="tag">'+esc(s.model)+'</span><br>':'')+s.duration_ms+' ms</div></div>').join('')}$$('.tab').forEach(t=>t.onclick=()=>{$$('.tab,.view').forEach(x=>x.classList.remove('active'));t.classList.add('active');$('#'+t.dataset.view).classList.add('active')});$('#refresh').onclick=()=>location.reload();$('#configForm').addEventListener('input',renderPreview);$('#configForm').onsubmit=async e=>{e.preventDefault();const n=x=>Number(get(x)),body={enabled:true,combo_model:get('combo_model').trim(),combo_aliases:get('combo_aliases').split(',').map(x=>x.trim()).filter(Boolean),primary_model:get('primary_model'),primary_context_tokens:n('primary_context_tokens'),primary_context_budget_tokens:n('primary_context_budget_tokens'),text_fallback_models:['text_fallback_0','text_fallback_1','text_fallback_2'].map(get).filter(Boolean),vision_primary_model:get('vision_primary_model'),vision_backup_model_1:get('vision_backup_model_1'),vision_backup_model_2:get('vision_backup_model_2'),vision_backup_model_3:get('vision_backup_model_3'),vision_context_limit:n('vision_context_limit'),vision_input_token_budget:n('vision_input_token_budget'),vision_output_tokens:n('vision_output_tokens'),vision_timeout_seconds:n('vision_timeout_seconds'),cache_ttl_seconds:n('cache_ttl_seconds'),cache_max_entries:n('cache_max_entries'),event_log_max_entries:n('event_log_max_entries'),on_vision_failure:get('strict_vision_failure')?'error':'text_only',strict_vision_failure:get('strict_vision_failure'),max_images_per_request:n('max_images_per_request'),max_concurrent_extractions:n('max_concurrent_extractions'),max_image_data_bytes:n('max_image_data_bytes'),allow_remote_image_urls:get('allow_remote_image_urls'),history_attachment_mode:get('history_attachment_mode'),history_attachment_compact_chars:n('history_attachment_compact_chars'),history_attachment_restore_max_attachments:n('history_attachment_restore_max_attachments'),auto_compression_enabled:get('auto_compression_enabled'),auto_compression_threshold_tokens:n('auto_compression_threshold_tokens'),auto_compression_target_tokens:n('auto_compression_target_tokens'),auto_compression_keep_recent_turns:n('auto_compression_keep_recent_turns'),auto_compression_model:get('auto_compression_model')};const note=$('#saveNote'),key=$('#managerKey').value.trim(),headers={'Content-Type':'application/json'};if(key)headers.Authorization='Bearer '+key;note.textContent='正在校验并保存配置…';try{const r=await fetch('/v0/management/plugins/glm-vision-combo/config',{method:'PUT',headers,body:JSON.stringify(body)});if(!r.ok){let detail='HTTP '+r.status;try{detail+=' · '+await r.text()}catch{}throw new Error(detail)}$('#managerKey').value='';note.textContent='保存成功，插件正在重新加载；页面即将刷新。';setTimeout(()=>location.reload(),1000)}catch(err){note.textContent='保存失败：'+err.message+'。请检查参数和管理密钥。'}};init();</script></body></html>`
}
