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

// The model list is requested only while a management page is being rendered.
// A brief cache prevents the plugin from adding a self-request to /v1/models on
// every browser refresh, while still reflecting OAuth/provider changes quickly.
var modelCatalogCache struct {
	sync.Mutex
	models    []string
	expiresAt time.Time
}

type dashboardConfig struct {
	Enabled                bool   `json:"enabled"`
	ComboModel             string `json:"combo_model"`
	PrimaryModel           string `json:"primary_model"`
	VisionPrimaryModel     string `json:"vision_primary_model"`
	VisionBackupModel1     string `json:"vision_backup_model_1"`
	VisionBackupModel2     string `json:"vision_backup_model_2"`
	VisionBackupModel3     string `json:"vision_backup_model_3"`
	VisionContextLimit     int    `json:"vision_context_limit"`
	VisionInputTokenBudget int    `json:"vision_input_token_budget"`
	VisionOutputTokens     int    `json:"vision_output_tokens"`
	VisionTimeoutSeconds   int    `json:"vision_timeout_seconds"`
	CacheTTLSeconds        int    `json:"cache_ttl_seconds"`
	CacheMaxEntries        int    `json:"cache_max_entries"`
	EventLogMaxEntries     int    `json:"event_log_max_entries"`
	OnVisionFailure        string `json:"on_vision_failure"`
	MaxImagesPerRequest    int    `json:"max_images_per_request"`
	MaxImageDataBytes      int    `json:"max_image_data_bytes"`
	AllowRemoteImageURLs   bool   `json:"allow_remote_image_urls"`
}

func dashboardConfigFrom(cfg runtimeConfig) dashboardConfig {
	return dashboardConfig{
		Enabled: cfg.Enabled, ComboModel: cfg.ComboModel, PrimaryModel: cfg.PrimaryModel,
		VisionPrimaryModel: cfg.VisionPrimaryModel, VisionBackupModel1: cfg.VisionBackupModel1,
		VisionBackupModel2: cfg.VisionBackupModel2, VisionBackupModel3: cfg.VisionBackupModel3,
		VisionContextLimit: cfg.VisionContextLimit, VisionInputTokenBudget: cfg.VisionInputTokenBudget,
		VisionOutputTokens: cfg.VisionOutputTokens, VisionTimeoutSeconds: cfg.VisionTimeoutSeconds,
		CacheTTLSeconds: cfg.CacheTTLSeconds, CacheMaxEntries: cfg.CacheMaxEntries,
		EventLogMaxEntries: cfg.EventLogMaxEntries, OnVisionFailure: cfg.OnVisionFailure,
		MaxImagesPerRequest: cfg.MaxImagesPerRequest, MaxImageDataBytes: cfg.MaxImageDataBytes,
		AllowRemoteImageURLs: cfg.AllowRemoteImageURLs,
	}
}

func managementJSONResponse(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{StatusCode: 200, Headers: map[string][]string{"content-type": {"application/json; charset=utf-8"}, "cache-control": {"no-store"}}, Body: body})
}

// currentModelCatalog returns only the CPA runtime /v1/models directory. It
// deliberately does not merge model names found in the YAML config: those
// names may be upstream/provider identifiers which CPA does not expose.
func currentModelCatalog(_ runtimeConfig) []string {
	raw, err := os.ReadFile(cpaConfigPath)
	if err != nil {
		return nil
	}
	return runtimeCPAModels(raw)
}

// runtimeCPAModels queries the already-local CPA API. The API key is read from
// CPA's mounted config solely to authorize this localhost request; it is never
// added to the response, event log, dashboard data, or plugin configuration.
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
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	set := make(map[string]struct{}, len(body.Data))
	for _, item := range body.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			set[id] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
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
	return `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>GLM Vision Combo</title>
<style>
:root{--ink:#172033;--muted:#64748b;--line:#d8e0ea;--paper:#fff;--ground:#f4f7fb;--blue:#255fce;--blue-soft:#edf4ff;--green:#13795b;--green-soft:#e9f7f0;--amber:#9a5d08;--amber-soft:#fff6df;--red:#b33d3d;--red-soft:#fff0f0}*{box-sizing:border-box}body{margin:0;background:var(--ground);color:var(--ink);font:14px ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}main{max-width:1320px;margin:0 auto;padding:28px}.top{display:flex;justify-content:space-between;gap:24px;align-items:flex-start;border-bottom:1px solid var(--line);padding-bottom:22px}.eyebrow{font-size:12px;letter-spacing:.12em;color:var(--blue);font-weight:700}.top h1{font-size:29px;letter-spacing:-.04em;margin:6px 0}.top p{margin:0;color:var(--muted)}.metrics{display:flex;gap:10px;flex-wrap:wrap}.metric{min-width:112px;background:var(--paper);border:1px solid var(--line);padding:10px 12px}.metric b{display:block;font-size:17px}.metric span{font-size:12px;color:var(--muted)}.tabs{display:flex;gap:18px;margin:22px 0 16px;border-bottom:1px solid var(--line)}.tab{appearance:none;border:0;background:none;padding:0 0 12px;font:inherit;color:var(--muted);cursor:pointer}.tab.active{color:var(--blue);font-weight:700;border-bottom:2px solid var(--blue)}.view{display:none}.view.active{display:block}.event-layout{display:grid;grid-template-columns:310px minmax(0,1fr);gap:18px}.panel{background:var(--paper);border:1px solid var(--line)}.panel-head{display:flex;justify-content:space-between;align-items:center;padding:14px 16px;border-bottom:1px solid var(--line)}.panel-head h2{font-size:14px;margin:0}.button{border:1px solid #b9c8e1;background:#fff;color:#244b91;padding:7px 10px;font:inherit;cursor:pointer}.button.primary{background:var(--blue);border-color:var(--blue);color:#fff}.event-list{max-height:650px;overflow:auto}.event-item{width:100%;text-align:left;border:0;border-bottom:1px solid #edf0f4;background:#fff;padding:13px 15px;cursor:pointer}.event-item:hover,.event-item.active{background:var(--blue-soft)}.event-title{display:flex;justify-content:space-between;gap:8px;font-weight:650}.event-meta{margin-top:5px;color:var(--muted);font-size:12px}.status{font-size:11px;padding:2px 6px;white-space:nowrap}.status.完成{background:var(--green-soft);color:var(--green)}.status.进行中{background:var(--blue-soft);color:var(--blue)}.status.失败{background:var(--red-soft);color:var(--red)}.detail{padding:20px}.detail-empty{padding:56px 24px;color:var(--muted);text-align:center}.route-strip{display:flex;align-items:center;gap:6px;flex-wrap:wrap;padding:14px 0 20px;border-bottom:1px solid var(--line)}.route-node{padding:7px 9px;border:1px solid var(--line);background:#fff;font-size:12px}.route-arrow{color:#9aa8ba}.stage{display:grid;grid-template-columns:145px 1fr 70px;gap:12px;border-bottom:1px solid #edf0f4;padding:14px 0}.stage-name{font-weight:650}.stage-detail{line-height:1.55;color:#334155;white-space:pre-wrap;word-break:break-word}.stage-meta{text-align:right;color:var(--muted);font-size:12px}.notice{padding:12px 14px;margin:14px 0;background:var(--amber-soft);color:#744709;border-left:3px solid #d79721;line-height:1.6}.config-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:0;border:1px solid var(--line);background:var(--paper)}.field{padding:15px;border-right:1px solid var(--line);border-bottom:1px solid var(--line)}.field:nth-child(2n){border-right:0}.field.full{grid-column:1/-1}.field label{display:block;font-weight:700;margin-bottom:7px}.field small{display:block;color:var(--muted);line-height:1.45;margin-top:6px}.field input,.field select{width:100%;border:1px solid #bfcbd9;background:#fff;padding:9px 10px;font:inherit;color:var(--ink)}.field.check{display:flex;gap:10px;align-items:flex-start}.field.check input{width:auto;margin-top:3px}.save-row{display:flex;align-items:center;gap:12px;margin-top:16px}.save-note{color:var(--muted);font-size:12px}.tag{font:12px ui-monospace,SFMono-Regular,Menlo,monospace;background:#f1f4f8;padding:3px 6px}.hidden{display:none}@media(max-width:820px){main{padding:18px}.top{display:block}.metrics{margin-top:16px}.event-layout{grid-template-columns:1fr}.event-list{max-height:280px}.config-grid{grid-template-columns:1fr}.field{border-right:0}.stage{grid-template-columns:1fr}.stage-meta{text-align:left}.field.full{grid-column:auto}}
</style></head><body><main><header class="top"><div><div class="eyebrow">组合路由 · 事件追踪</div><h1>GLM Vision Combo</h1><p>图片只进入视觉候选链；视觉记忆转写后，始终由 <span class="tag" id="primaryName"></span> 完成最终任务。</p></div><div class="metrics"><div class="metric"><b id="eventCount">0</b><span>内存事件</span></div><div class="metric"><b id="visionName">—</b><span>首选视觉模型</span></div><div class="metric"><b id="contextLimit">—</b><span>视觉上下文上限</span></div></div></header>
<nav class="tabs"><button class="tab active" data-view="events">请求事件</button><button class="tab" data-view="settings">配置编辑</button></nav>
<section id="events" class="view active"><div class="event-layout"><aside class="panel"><div class="panel-head"><h2>最近组合请求</h2><button class="button" id="refresh">刷新</button></div><div id="eventList" class="event-list"></div></aside><section class="panel"><div class="panel-head"><h2>视觉处理过程</h2><span id="selectedId" class="tag">未选择</span></div><div id="eventDetail" class="detail"></div></section></div></section>
<section id="settings" class="view"><div class="notice">模型选项严格来自 CPA 当前运行时模型目录（<code>/v1/models</code>），不会混入配置文件中的上游名称。目录每 15 秒更新一次；若 CPA 暂时无法读取，则下拉框不会展示旧的或推测的模型。保存将调用 CPA 的插件配置接口并自动重新加载插件；事件日志仅保存在插件内存中，重启后会清空。</div><form id="configForm"><div class="config-grid" id="configGrid"></div><div class="field full"><label for="managerKey">CPA Manager Plus 管理密钥</label><input id="managerKey" type="password" autocomplete="off" placeholder="仅用于本次保存请求"><small>CPAM 出于安全原因不会自动把登录密钥传入 iframe。此值不会写入插件配置、不会存入浏览器，保存后即丢弃。</small></div><div class="save-row"><button class="button primary" type="submit">保存并重新加载</button><span id="saveNote" class="save-note">保存前不会改变当前流量。</span></div></form></section></main>
<script>const DATA=` + jsonForScript + `;const $=s=>document.querySelector(s);const esc=s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));const config=DATA.config,models=[...new Set(DATA.models)].sort(),events=DATA.events||[];let selected=events[0]?.id||'';const fields=[['combo_model','对外组合模型','text','其他 Agent 调用的唯一模型名。'],['primary_model','首选文本模型','model','无图、以及完成视觉转写后均由它负责最终回答。'],['vision_primary_model','首选视觉模型','model','检测到图片后的第一个候选模型。'],['vision_backup_model_1','备用视觉模型 1','model','首选视觉模型报错、超时或无有效结果时自动尝试。'],['vision_backup_model_2','备用视觉模型 2','model','第二个视觉备用位。'],['vision_backup_model_3','备用视觉模型 3','model','第三个视觉备用位。'],['vision_context_limit','视觉模型上下文上限','number','共同上限；插件预检后不会把超限内容发送给视觉模型。'],['vision_input_token_budget','视觉文本窗口预算','number','每次识图附带的相关文本预算，不会传入完整长会话。'],['vision_output_tokens','视觉识别输出上限','number','视觉描述最大长度。描述越短，GLM 的可用上下文越多。'],['vision_timeout_seconds','单个视觉模型超时（秒）','number','超时后自动尝试下一个视觉候选。'],['cache_ttl_seconds','视觉记忆缓存时长（秒）','number','同一图片命中缓存时不重复调用视觉模型。'],['cache_max_entries','视觉记忆缓存条数','number','内存上限；达到后淘汰较早条目。'],['event_log_max_entries','事件日志保留条数','number','只保存脱敏的路由过程；重启插件后清空。'],['max_images_per_request','单请求最多识别新图片数','number','仅限制未缓存的新图片。缓存的历史图片会全部替换为视觉记忆；超过上限整次请求被拦截，原图不会发给 GLM。'],['max_image_data_bytes','内嵌图片大小上限（字节）','number','限制 data URL 图片，避免异常大请求。'],['on_vision_failure','全部视觉模型失败时','select','“返回错误”可避免 GLM 在没有视觉信息时猜测。']];function opts(value){return '<option value="">不选择</option>'+models.map(m=>'<option value="'+esc(m)+'" '+(m===value?'selected':'')+'>'+esc(m)+'</option>').join('')}function field([key,label,type,help]){const v=config[key]??'';let input=type==='model'?'<select name="'+key+'">'+opts(v)+'</select>':type==='select'?'<select name="'+key+'"><option value="error" '+(v==='error'?'selected':'')+'>返回错误（推荐）</option><option value="text_only" '+(v==='text_only'?'selected':'')+'>仅继续文本内容</option></select>':'<input name="'+key+'" type="'+type+'" value="'+esc(v)+'">';return '<div class="field"><label>'+label+'</label>'+input+'<small>'+help+'</small></div>'}function renderConfig(){let html=fields.map(field).join('');html+='<div class="field full check"><input id="allowRemote" type="checkbox" '+(config.allow_remote_image_urls?'checked':'')+'><div><label for="allowRemote">允许远程图片 URL</label><small>关闭后仅接受受大小限制的 data URL 图片。建议仅在需要读取外部图片时开启。</small></div></div>';$('#configGrid').innerHTML=html}function renderEvents(){ $('#eventCount').textContent=events.length;$('#visionName').textContent=config.vision_primary_model||'未设置';$('#contextLimit').textContent=(config.vision_context_limit||0).toLocaleString();$('#primaryName').textContent=config.primary_model||'未设置';const list=$('#eventList');if(!events.length){list.innerHTML='<div class="detail-empty">尚无组合请求。向 <span class="tag">'+esc(config.combo_model)+'</span> 发起一次文本或图片请求后，这里会显示完整过程。</div>';$('#eventDetail').innerHTML='<div class="detail-empty">事件详情会显示：候选模型、回退原因、视觉记忆摘要与 GLM 最终阶段。</div>';return}list.innerHTML=events.map(e=>'<button class="event-item '+(e.id===selected?'active':'')+'" data-id="'+esc(e.id)+'"><div class="event-title"><span>'+esc(e.id)+'</span><span class="status '+esc(e.status)+'">'+esc(e.status)+'</span></div><div class="event-meta">'+(e.image_count?'含 '+e.image_count+' 张图片':'纯文本')+' · '+(e.stream?'流式':'非流式')+'<br>'+new Date(e.started_at).toLocaleTimeString()+'</div></button>').join('');list.querySelectorAll('[data-id]').forEach(b=>b.onclick=()=>{selected=b.dataset.id;renderEvents()});const e=events.find(x=>x.id===selected)||events[0];$('#selectedId').textContent=e.id;const flow=['收到请求','检查图片','视觉候选链','视觉记忆','主文本模型'];$('#eventDetail').innerHTML='<div class="route-strip">'+flow.map((x,i)=>'<span class="route-node">'+x+'</span>'+(i<flow.length-1?'<span class="route-arrow">→</span>':'')).join('')+'</div>'+(e.error?'<div class="notice">本次请求失败：'+esc(e.error)+'</div>':'')+e.stages.map(s=>'<div class="stage"><div><div class="stage-name">'+esc(s.name)+'</div><span class="status '+esc(s.status)+'">'+esc(s.status)+'</span></div><div class="stage-detail">'+esc(s.detail||'—')+'</div><div class="stage-meta">'+(s.model?'<span class="tag">'+esc(s.model)+'</span><br>':'')+s.duration_ms+' ms</div></div>').join('')}document.querySelectorAll('.tab').forEach(tab=>tab.onclick=()=>{document.querySelectorAll('.tab,.view').forEach(x=>x.classList.remove('active'));tab.classList.add('active');$('#'+tab.dataset.view).classList.add('active')});$('#refresh').onclick=()=>location.reload();$('#configForm').onsubmit=async e=>{e.preventDefault();const f=new FormData(e.currentTarget),body={enabled:true,allow_remote_image_urls:$('#allowRemote').checked};fields.forEach(([k,,type])=>{let v=f.get(k);body[k]=type==='number'?Number(v):v});const note=$('#saveNote'),key=$('#managerKey').value.trim(),headers={'Content-Type':'application/json'};if(key)headers.Authorization='Bearer '+key;note.textContent='正在保存…';try{const r=await fetch('/v0/management/plugins/glm-vision-combo/config',{method:'PUT',headers,body:JSON.stringify(body)});if(!r.ok)throw new Error('HTTP '+r.status);$('#managerKey').value='';note.textContent='已保存，插件配置正在重新加载。';setTimeout(()=>location.reload(),900)}catch(err){note.textContent='保存未完成：'+err.message+'。请输入 CPA Manager Plus 管理密钥后重试。'}};renderConfig();renderEvents();</script></body></html>`
}
