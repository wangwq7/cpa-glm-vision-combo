package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);
typedef struct { uint32_t abi_version; void* host_ctx; cliproxy_host_call_fn call; cliproxy_host_free_fn free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
static const cliproxy_host_api* stored_host;
static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }
static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) { if (!stored_host || !stored_host->call) return 1; return stored_host->call(stored_host->host_ctx, method, request, request_len, response); }
static void free_host_buffer(void* ptr, size_t len) { if (stored_host && stored_host->free_buffer && ptr) stored_host->free_buffer(ptr, len); }
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginID = "glm-vision-combo"

var pluginVersion = "0.2.6"
var configured atomic.Value
var telemetry = newEventStore(100)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}
type capabilities struct {
	ModelProvider         bool     `json:"model_provider"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	ManagementAPI         bool     `json:"management_api"`
}
type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}
type rpcRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type hostModelRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}
type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload"`
}
type streamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", false))
		return 1
	}
	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), payload)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error(), false))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(payload); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginID, Models: []pluginapi.ModelInfo{comboModel(currentConfig())}})
	case pluginabi.MethodModelRoute:
		return routeModel(payload)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodExecutorExecute:
		return execute(payload)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(payload)
	case pluginabi.MethodExecutorCountTokens:
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return managementHandle(payload)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, false), nil
	}
}

func configure(raw []byte) error {
	cfg := defaultPluginConfig()
	if len(raw) > 0 {
		var request lifecycleRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		if len(request.ConfigYAML) > 0 {
			if err := yaml.Unmarshal(request.ConfigYAML, &cfg); err != nil {
				return err
			}
		}
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return err
	}
	telemetry.setLimit(normalized.EventLogMaxEntries)
	configured.Store(runtimeConfig{pluginConfig: normalized, cache: newMemoCache(normalized.CacheMaxEntries), events: telemetry})
	return nil
}
func currentConfig() runtimeConfig {
	if raw := configured.Load(); raw != nil {
		return raw.(runtimeConfig)
	}
	cfg, _ := normalizeConfig(defaultPluginConfig())
	r := runtimeConfig{pluginConfig: cfg, cache: newMemoCache(cfg.CacheMaxEntries), events: telemetry}
	configured.Store(r)
	return r
}
func metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: "GLM Vision Combo", Version: pluginVersion, Author: "Local plugin", GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI", ConfigFields: []pluginapi.ConfigField{
		{Name: "combo_model", Type: pluginapi.ConfigFieldTypeString, Description: "对外暴露的虚拟模型名。Agent 只调用这个名字。"},
		{Name: "primary_model", Type: pluginapi.ConfigFieldTypeString, Description: "最终回答始终使用的文本模型；推荐 glm-5.2。"},
		{Name: "vision_primary_model", Type: pluginapi.ConfigFieldTypeString, Description: "首选视觉模型。检测到图片时先调用它，成功后仍由主文本模型完成最终回答。"},
		{Name: "vision_backup_model_1", Type: pluginapi.ConfigFieldTypeString, Description: "备用视觉模型 1。首选模型超时、报错或不可用时自动尝试。"},
		{Name: "vision_backup_model_2", Type: pluginapi.ConfigFieldTypeString, Description: "备用视觉模型 2。"},
		{Name: "vision_backup_model_3", Type: pluginapi.ConfigFieldTypeString, Description: "备用视觉模型 3。"},
		{Name: "vision_context_limit", Type: pluginapi.ConfigFieldTypeInteger, Description: "上述视觉模型共同的最大上下文。默认 256K；插件会在调用前拦截超限请求。"},
		{Name: "vision_models", Type: pluginapi.ConfigFieldTypeArray, Description: "高级模式：按优先级定义 {model, context_limit, enabled}。填写首选/备用字段时，它们优先。"},
		{Name: "vision_input_token_budget", Type: pluginapi.ConfigFieldTypeInteger, Description: "单次视觉识别可带入的文本预算；不会把完整长会话发送给视觉模型。"},
		{Name: "vision_output_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "每次视觉识别的最大输出 token。"},
		{Name: "vision_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "每个视觉候选模型的超时时间。"},
		{Name: "cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "同一图片视觉记忆的内存缓存时间；更新或重启后自动清空。"},
		{Name: "cache_max_entries", Type: pluginapi.ConfigFieldTypeInteger, Description: "视觉记忆内存缓存的最大条数，达到上限后淘汰较早条目。"},
		{Name: "event_log_max_entries", Type: pluginapi.ConfigFieldTypeInteger, Description: "组合请求事件日志保留条数。日志只保留处理阶段和视觉记忆摘要，不保存原始图片链接。"},
		{Name: "on_vision_failure", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"error", "text_only"}, Description: "所有视觉模型失败时，返回错误或仅继续文本内容。"},
		{Name: "max_images_per_request", Type: pluginapi.ConfigFieldTypeInteger, Description: "单次请求中允许调用视觉模型的最多未缓存新图片数。历史缓存图片会全部替换为视觉记忆；超过上限将拒绝整次请求，绝不向主文本模型透传原图。"},
		{Name: "max_image_data_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "data URL 内嵌图片的最大字节数，防止异常大图片占用内存。"},
		{Name: "allow_remote_image_urls", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否允许视觉模型读取 http/https 图片 URL。"},
	}}
}
func pluginRegistration() registration {
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: metadata(), Capabilities: capabilities{ModelProvider: true, ModelRouter: true, Executor: true, ExecutorModelScope: string(pluginapi.ExecutorModelScopeStatic), ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, ManagementAPI: true}}
}
func comboModel(cfg runtimeConfig) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{ID: cfg.ComboModel, Object: "model", OwnedBy: pluginID, Type: "chat", DisplayName: "GLM-5.2 Vision Combo", Description: "Visual preprocessing with GLM as the final text model.", ContextLength: 1000000, MaxCompletionTokens: 16384, SupportedGenerationMethods: []string{"chat"}, SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"text"}, UserDefined: true}
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := currentConfig()
	if !cfg.Enabled || req.SourceFormat != "openai" || strings.TrimSpace(req.RequestedModel) != cfg.ComboModel {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetSelf, Reason: "glm_vision_combo_orchestration"})
}
func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := currentConfig()
	event := cfg.events.begin(req.Model, cfg.PrimaryModel, false)
	started := time.Now()
	cfg.events.stage(event, "接收组合请求", "完成", req.Model, "已识别 OpenAI 请求，开始检查多模态内容。", started)
	body, images, err := preparePrimaryBody(req.OriginalRequest, cfg, req.HostCallbackID, event)
	cfg.events.setImageCount(event, images)
	if images == 0 {
		cfg.events.stage(event, "纯文本直达", "完成", cfg.PrimaryModel, "未检测到图片，跳过视觉候选链。", time.Now())
	}
	if err != nil {
		cfg.events.stage(event, "多模态预处理", "失败", "", err.Error(), time.Now())
		cfg.events.finish(event, err)
		return nil, err
	}
	primaryStarted := time.Now()
	cfg.events.stage(event, "交给主文本模型", "完成", cfg.PrimaryModel, "图片已替换为结构化视觉记忆，已提交给主文本模型；主模型不再接收原始图片。", primaryStarted)
	response, err := hostExecute(req.HostCallbackID, cfg.PrimaryModel, body, false)
	if err != nil {
		cfg.events.stage(event, "主文本模型返回", "失败", cfg.PrimaryModel, err.Error(), primaryStarted)
		cfg.events.finish(event, err)
		return nil, err
	}
	cfg.events.stage(event, "主文本模型返回", "完成", cfg.PrimaryModel, "已生成最终非流式回答。", primaryStarted)
	cfg.events.finish(event, nil)
	return okEnvelope(pluginapi.ExecutorResponse{Payload: response.Body, Headers: response.Headers})
}
func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required", false), nil
	}
	cfg := currentConfig()
	event := cfg.events.begin(req.Model, cfg.PrimaryModel, true)
	go func() {
		started := time.Now()
		cfg.events.stage(event, "接收组合请求", "完成", req.Model, "已识别流式 OpenAI 请求，开始检查多模态内容。", started)
		body, images, err := preparePrimaryBody(req.OriginalRequest, cfg, req.HostCallbackID, event)
		cfg.events.setImageCount(event, images)
		if images == 0 {
			cfg.events.stage(event, "纯文本直达", "完成", cfg.PrimaryModel, "未检测到图片，跳过视觉候选链。", time.Now())
		}
		if err == nil {
			primaryStarted := time.Now()
			cfg.events.stage(event, "交给主文本模型", "完成", cfg.PrimaryModel, "图片已替换为结构化视觉记忆，已提交主模型并开始透传输出流。", primaryStarted)
			err = forwardPrimaryStream(req.StreamID, req.HostCallbackID, cfg.PrimaryModel, body)
			if err != nil {
				cfg.events.stage(event, "主文本流结束", "失败", cfg.PrimaryModel, err.Error(), primaryStarted)
			} else {
				cfg.events.stage(event, "主文本流结束", "完成", cfg.PrimaryModel, "流式输出已完整透传。", primaryStarted)
			}
		} else {
			cfg.events.stage(event, "多模态预处理", "失败", "", err.Error(), time.Now())
		}
		cfg.events.finish(event, err)
		closePluginStream(req.StreamID, err)
	}()
	return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

func preparePrimaryBody(raw []byte, cfg runtimeConfig, callbackID string, event *comboEvent) ([]byte, int, error) {
	if len(raw) == 0 {
		return nil, 0, fmt.Errorf("original OpenAI request is missing")
	}
	return transformOpenAIRequest(raw, cfg, func(asset visualAsset, contextText string) (string, error) {
		return describeImage(cfg, callbackID, asset, contextText, event)
	})
}
func describeImage(cfg runtimeConfig, callbackID string, asset visualAsset, contextText string, event *comboEvent) (string, error) {
	key := visualCacheKey(cfg, asset)
	if cached, ok := cfg.cache.get(key); ok {
		cfg.events.stage(event, "读取视觉记忆缓存", "完成", "缓存", "同一图片命中本地内存缓存，未再次调用视觉模型。", time.Now())
		return cached, nil
	}
	cfg.events.stage(event, "进入视觉候选链", "完成", "", fmt.Sprintf("视觉文本窗口预算 %d token；原图不会发送给主文本模型。", cfg.VisionInputTokenBudget), time.Now())
	var lastErr error
	for _, candidate := range cfg.VisionModels {
		if !candidate.active() {
			cfg.events.stage(event, "视觉候选跳过", "跳过", candidate.Model, "该候选模型已在配置中停用。", time.Now())
			continue
		}
		if estimateTokens(contextText)+cfg.VisionImageTokenReserve+cfg.VisionOutputTokens > candidate.ContextLimit {
			lastErr = fmt.Errorf("vision model %s skipped: projected context exceeds %d", candidate.Model, candidate.ContextLimit)
			cfg.events.stage(event, "视觉上下文预检", "跳过", candidate.Model, fmt.Sprintf("预测上下文超过该模型的 %d token 上限，未发送请求。", candidate.ContextLimit), time.Now())
			continue
		}
		candidateStarted := time.Now()
		request := makeVisionRequest(candidate.Model, cfg.VisionPrompt, contextText, asset.URL, cfg.VisionOutputTokens)
		result, err := hostExecuteWithTimeout(callbackID, candidate.Model, request, cfg.VisionTimeoutSeconds)
		if err != nil {
			lastErr = err
			cfg.events.stage(event, "视觉候选调用", "失败", candidate.Model, "调用失败，继续尝试下一个候选："+err.Error(), candidateStarted)
			continue
		}
		description := extractVisionText(result.Body)
		if description == "" {
			lastErr = fmt.Errorf("vision model %s returned no usable text", candidate.Model)
			cfg.events.stage(event, "视觉候选调用", "失败", candidate.Model, "返回为空，继续尝试下一个候选。", candidateStarted)
			continue
		}
		cfg.cache.set(key, description, time.Duration(cfg.CacheTTLSeconds)*time.Second)
		cfg.events.stage(event, "视觉识别完成", "完成", candidate.Model, "已提取视觉记忆：\n"+description, candidateStarted)
		cfg.events.stage(event, "注入视觉记忆", "完成", cfg.PrimaryModel, "原始图片片段已替换为上方视觉记忆文本，随后继续由主文本模型完成任务。", time.Now())
		return description, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no enabled visual model is configured")
	}
	return "", lastErr
}

func hostExecute(callbackID, model string, body []byte, stream bool) (pluginapi.HostModelExecutionResponse, error) {
	raw, err := callHost(pluginabi.MethodHostModelExecute, hostModelRequest{HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: model, Stream: stream, Body: body}, HostCallbackID: callbackID})
	if err != nil {
		return pluginapi.HostModelExecutionResponse{}, err
	}
	var response pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return response, err
	}
	if response.StatusCode >= 400 {
		return response, fmt.Errorf("upstream model %s returned HTTP %d", model, response.StatusCode)
	}
	return response, nil
}
func hostExecuteWithTimeout(callbackID, model string, body []byte, seconds int) (pluginapi.HostModelExecutionResponse, error) {
	type result struct {
		response pluginapi.HostModelExecutionResponse
		err      error
	}
	done := make(chan result, 1)
	go func() { r, e := hostExecute(callbackID, model, body, false); done <- result{r, e} }()
	select {
	case out := <-done:
		return out.response, out.err
	case <-time.After(time.Duration(seconds) * time.Second):
		return pluginapi.HostModelExecutionResponse{}, fmt.Errorf("vision model %s timed out after %ds", model, seconds)
	}
}
func forwardPrimaryStream(streamID, callbackID, model string, body []byte) error {
	raw, err := callHost(pluginabi.MethodHostModelExecuteStream, hostModelRequest{HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{EntryProtocol: "openai", ExitProtocol: "openai", Model: model, Stream: true, Body: body}, HostCallbackID: callbackID})
	if err != nil {
		return err
	}
	var started pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(raw, &started); err != nil {
		return err
	}
	if started.StatusCode >= 400 {
		return fmt.Errorf("primary model returned HTTP %d", started.StatusCode)
	}
	if started.StreamID == "" {
		return fmt.Errorf("primary stream id is missing")
	}
	defer func() {
		_, _ = callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: started.StreamID})
	}()
	for {
		chunkRaw, err := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: started.StreamID})
		if err != nil {
			return err
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			return err
		}
		if chunk.Error != "" {
			return fmt.Errorf("primary stream error: %s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if _, err = callHost(pluginabi.MethodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: chunk.Payload}); err != nil {
				return err
			}
		}
		if chunk.Done {
			return nil
		}
	}
}
func closePluginStream(streamID string, err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: message})
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}
type managementRoute struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
}
type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}
type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: "/glm-vision-combo/events"},
			{Method: http.MethodGet, Path: "/glm-vision-combo/model-catalog"},
		},
		Resources: []resourceRoute{{Path: "/open", Menu: "GLM Vision Combo", Description: "查看组合请求事件、视觉处理链路，并编辑配置。"}},
	}
}
func managementHandle(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := currentConfig()
	switch {
	case strings.HasSuffix(req.Path, "/events"):
		return managementJSONResponse(cfg.events.snapshot())
	case strings.HasSuffix(req.Path, "/model-catalog"):
		return managementJSONResponse(currentModelCatalog(cfg))
	default:
		return okEnvelope(managementResponse{StatusCode: 200, Headers: http.Header{"content-type": []string{"text/html; charset=utf-8"}, "cache-control": []string{"no-store"}}, Body: []byte(managementHTML(cfg))})
	}
}
func managementHTMLLegacy(cfg runtimeConfig) string {
	models := make([]string, 0, len(cfg.VisionModels))
	for _, m := range cfg.VisionModels {
		models = append(models, fmt.Sprintf("<li><code>%s</code> · %dK 上下文 · %s</li>", htmlEscape(m.Model), m.ContextLimit/1024, map[bool]string{true: "启用", false: "停用"}[m.active()]))
	}
	if len(models) == 0 {
		models = append(models, "<li>尚未配置视觉模型。请在“插件管理 → GLM Vision Combo → 配置”中填写 <code>vision_models</code>。</li>")
	}
	return "<!doctype html><html lang=zh-CN><meta charset=utf-8><meta name=viewport content='width=device-width,initial-scale=1'><title>GLM Vision Combo</title><style>body{margin:0;padding:28px;font:14px system-ui,-apple-system,BlinkMacSystemFont,Segoe UI,sans-serif;background:var(--cpa-bg,#f8fafc);color:var(--cpa-fg,#172033)}main{max-width:960px;margin:auto}.hero,.card{background:var(--cpa-card,#fff);border:1px solid #dce3ee;border-radius:14px;padding:22px;margin:14px 0;box-shadow:0 6px 20px #0f172a0a}h1{margin:0 0 7px;font-size:25px}h2{font-size:17px;margin:0 0 12px}.pill{display:inline-block;background:#e8f0ff;color:#2456a8;border-radius:999px;padding:4px 9px;font-size:12px}code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;background:#edf1f7;padding:2px 5px;border-radius:5px}.flow{display:flex;gap:8px;align-items:center;flex-wrap:wrap}.box{padding:10px 12px;border:1px solid #cbd5e1;border-radius:8px;background:#f8fafc}.arrow{color:#64748b}.warn{color:#9a3412;background:#fff7ed;padding:10px;border-radius:8px}li{margin:7px 0}</style><main><section class=hero><span class=pill>插件状态：" + map[bool]string{true: "已启用", false: "已停用"}[cfg.Enabled] + "</span><h1>GLM Vision Combo</h1><p>对外模型 <code>" + htmlEscape(cfg.ComboModel) + "</code>；最终回答模型 <code>" + htmlEscape(cfg.PrimaryModel) + "</code>。</p></section><section class=card><h2>路由预览</h2><div class=flow><span class=box>Agent 请求</span><span class=arrow>→</span><span class=box>检测图片</span><span class=arrow>→</span><span class=box>视觉模型链</span><span class=arrow>→</span><span class=box>视觉记忆</span><span class=arrow>→</span><span class=box>" + htmlEscape(cfg.PrimaryModel) + " 最终回答</span></div><p>纯文本将直接转发到主模型。视觉模型从不成为会话主模型。</p></section><section class=card><h2>视觉候选链</h2><ol>" + strings.Join(models, "") + "</ol><p class=warn>视觉调用最多带入 " + fmt.Sprint(cfg.VisionInputTokenBudget) + " 个文本 token 预算，并在每个候选模型上下文上限前预检；因此不会把一兆历史直接送入 256K 视觉模型。</p></section><section class=card><h2>配置</h2><p>请在 CPAM 的“插件管理 → GLM Vision Combo → 配置”保存设置。该页面展示实时生效配置；保存后 CPA 会自动重载插件配置。</p><p>视觉链示例：<code>[{\"model\":\"your-vision-model\",\"context_limit\":262144}]</code></p></section></main></html>"
}
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return r.Replace(s)
}
func callHost(method string, payload any) (json.RawMessage, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(encoded) > 0 {
		req = (*C.uint8_t)(C.CBytes(encoded))
		defer C.free(unsafe.Pointer(req))
	}
	if C.call_host_api(cMethod, req, C.size_t(len(encoded)), &response) != 0 {
		return nil, fmt.Errorf("host call %s failed", method)
	}
	if response.ptr == nil {
		return nil, fmt.Errorf("host call %s returned empty response", method)
	}
	defer C.free_host_buffer(response.ptr, response.len)
	raw := C.GoBytes(response.ptr, C.int(response.len))
	var reply envelope
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		if reply.Error == nil {
			return nil, fmt.Errorf("host call %s failed", method)
		}
		return nil, fmt.Errorf("%s", reply.Error.Message)
	}
	return reply.Result, nil
}
func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}
func errorEnvelope(code, message string, retry bool) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &rpcError{Code: code, Message: message, Retryable: retry}})
	return raw
}
func _unused(_ context.Context) {}
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
