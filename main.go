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

var pluginVersion = "0.4.4"
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
func cliproxyPluginShutdown() {
	if raw := configured.Load(); raw != nil {
		raw.(runtimeConfig).cache.close()
	}
}

func handleMethod(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(payload); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return okEnvelope(pluginapi.ModelResponse{Provider: pluginID, Models: comboModels(currentConfig())})
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
	var cache *memoCache
	if previous := configured.Load(); previous != nil {
		old := previous.(runtimeConfig).cache
		if old.compatible(normalized.CachePath) {
			cache = old
			cache.setLimit(normalized.CacheMaxEntries)
		} else {
			old.close()
		}
	}
	if cache == nil {
		cache = newMemoCache(normalized.CacheMaxEntries, normalized.CachePath)
	}
	configured.Store(runtimeConfig{pluginConfig: normalized, cache: cache, events: telemetry})
	return nil
}
func currentConfig() runtimeConfig {
	if raw := configured.Load(); raw != nil {
		return raw.(runtimeConfig)
	}
	cfg, _ := normalizeConfig(defaultPluginConfig())
	r := runtimeConfig{pluginConfig: cfg, cache: newMemoCache(cfg.CacheMaxEntries, cfg.CachePath), events: telemetry}
	configured.Store(r)
	return r
}
func metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: "GLM Vision Bridge", Version: pluginVersion, Author: "wangwq7", GitHubRepository: "https://github.com/wangwq7/cpa-glm-vision-combo", ConfigFields: []pluginapi.ConfigField{
		{Name: "combo_model", Type: pluginapi.ConfigFieldTypeString, Description: "对外暴露的唯一虚拟模型名。"},
		{Name: "primary_model", Type: pluginapi.ConfigFieldTypeString, Description: "最终回答始终优先使用的文本模型。"},
		{Name: "primary_context_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "主文本模型理论上下文上限。"},
		{Name: "primary_context_budget_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "主模型实际工作预算，必须低于理论上限。"},
		{Name: "text_fallback_models", Type: pluginapi.ConfigFieldTypeArray, Description: "主文本模型失败且尚未输出内容时依次尝试的备用模型。"},
		{Name: "vision_primary_model", Type: pluginapi.ConfigFieldTypeString, Description: "首选视觉模型；只负责识别，不负责最终回答。"},
		{Name: "vision_backup_model_1", Type: pluginapi.ConfigFieldTypeString, Description: "备用视觉模型 1。"},
		{Name: "vision_backup_model_2", Type: pluginapi.ConfigFieldTypeString, Description: "备用视觉模型 2。"},
		{Name: "vision_backup_model_3", Type: pluginapi.ConfigFieldTypeString, Description: "备用视觉模型 3。"},
		{Name: "vision_context_limit", Type: pluginapi.ConfigFieldTypeInteger, Description: "兼容模式下视觉候选共享的上下文上限。"},
		{Name: "vision_models", Type: pluginapi.ConfigFieldTypeArray, Description: "高级视觉链：model、context_limit、context_budget、timeout_seconds、max_output_tokens、enabled。"},
		{Name: "vision_input_token_budget", Type: pluginapi.ConfigFieldTypeInteger, Description: "视觉请求仅携带当前问题附近文字的最大预算。"},
		{Name: "vision_output_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "视觉识别输出上限。"},
		{Name: "vision_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "视觉流建立后、识别阶段的可取消超时时间。"},
		{Name: "vision_cancel_grace_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "调用 stream_close 后等待 Host 确认流已结束的最大秒数；未确认时不会启动备用模型。"},
		{Name: "cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "视觉记忆和历史摘要的持久缓存时长。"},
		{Name: "cache_max_entries", Type: pluginapi.ConfigFieldTypeInteger, Description: "持久缓存最大条数，使用 LRU 淘汰。"},
		{Name: "cache_path", Type: pluginapi.ConfigFieldTypeString, Description: "缓存文件路径。"},
		{Name: "event_log_max_entries", Type: pluginapi.ConfigFieldTypeInteger, Description: "内存事件日志保留条数，不保存原图。"},
		{Name: "on_vision_failure", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"error", "text_only"}, Description: "所有视觉模型失败时的兼容策略。"},
		{Name: "strict_vision_failure", Type: pluginapi.ConfigFieldTypeBoolean, Description: "所有视觉候选失败时直接报错。"},
		{Name: "max_images_per_request", Type: pluginapi.ConfigFieldTypeInteger, Description: "当前轮允许完整识别的最大图片数。"},
		{Name: "max_concurrent_extractions", Type: pluginapi.ConfigFieldTypeInteger, Description: "多张图片的并发识别数。"},
		{Name: "max_image_data_bytes", Type: pluginapi.ConfigFieldTypeInteger, Description: "data URL 图片解码后的真实最大字节数。"},
		{Name: "allow_remote_image_urls", Type: pluginapi.ConfigFieldTypeBoolean, Description: "是否允许读取 http/https 图片 URL。"},
		{Name: "history_attachment_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"onDemand", "retain"}, Description: "历史图片按需恢复或完整保留。"},
		{Name: "history_attachment_compact_chars", Type: pluginapi.ConfigFieldTypeInteger, Description: "无关轮中的历史图片短摘要字符数。"},
		{Name: "history_attachment_restore_max_attachments", Type: pluginapi.ConfigFieldTypeInteger, Description: "明确引用图片时最多恢复的历史图片数。"},
		{Name: "auto_compression_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "达到阈值后自动压缩较早对话。"},
		{Name: "auto_compression_threshold_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "自动压缩触发阈值。"},
		{Name: "auto_compression_target_tokens", Type: pluginapi.ConfigFieldTypeInteger, Description: "历史摘要目标 token。"},
		{Name: "auto_compression_keep_recent_turns", Type: pluginapi.ConfigFieldTypeInteger, Description: "压缩时保留原文的最近轮数。"},
		{Name: "auto_compression_model", Type: pluginapi.ConfigFieldTypeString, Description: "压缩模型；留空使用首选文本模型。"},
	}}
}
func pluginRegistration() registration {
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: metadata(), Capabilities: capabilities{ModelProvider: true, ModelRouter: true, Executor: true, ExecutorModelScope: string(pluginapi.ExecutorModelScopeStatic), ExecutorInputFormats: []string{"openai", "claude"}, ExecutorOutputFormats: []string{"openai", "claude"}, ManagementAPI: true}}
}
func comboModels(cfg runtimeConfig) []pluginapi.ModelInfo {
	// Only expose the single configured combo_model. Aliases are ignored for
	// /v1/models so clients always see one public entry point.
	name := strings.TrimSpace(cfg.ComboModel)
	if name == "" {
		return nil
	}
	return []pluginapi.ModelInfo{{
		ID: name, Object: "model", OwnedBy: pluginID, Type: "chat",
		DisplayName:   "GLM Vision Bridge",
		Description:   "视觉模型只负责转写；最终任务始终由首选文本模型及其文本备用链完成。",
		ContextLength: int64(cfg.PrimaryContextTokens), MaxCompletionTokens: 16384,
		SupportedGenerationMethods: []string{"chat"},
		SupportedInputModalities:   []string{"text", "image"},
		SupportedOutputModalities:  []string{"text"},
		UserDefined:                true,
	}}
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := currentConfig()
	requested := strings.TrimSpace(req.RequestedModel)
	// Route only the single public model name. Legacy aliases are no longer accepted.
	matched := requested != "" && requested == strings.TrimSpace(cfg.ComboModel)
	protocol := normalizeProtocol(req.SourceFormat)
	if !cfg.Enabled || !isSupportedProtocol(protocol) || !matched {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetSelf, Reason: "glm_vision_combo_orchestration"})
}

func normalizeProtocol(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func isSupportedProtocol(value string) bool {
	return value == "openai" || value == "claude"
}

func executorProtocol(req rpcExecutorRequest) (string, error) {
	protocol := normalizeProtocol(req.SourceFormat)
	if protocol == "" {
		protocol = normalizeProtocol(req.Format)
	}
	if !isSupportedProtocol(protocol) {
		return "", fmt.Errorf("unsupported executor protocol %q", protocol)
	}
	return protocol, nil
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	protocol, err := executorProtocol(req)
	if err != nil {
		return nil, err
	}
	cfg := currentConfig()
	event := cfg.events.begin(req.Model, cfg.PrimaryModel, false)
	started := time.Now()
	cfg.events.stage(event, "接收组合请求", "完成", req.Model, fmt.Sprintf("已识别 %s 请求，开始检查多模态内容。", protocol), started)
	body, images, err := preparePrimaryBody(req.OriginalRequest, protocol, cfg, req.HostCallbackID, event)
	cfg.events.setImageCount(event, images)
	if images == 0 {
		cfg.events.stage(event, "纯文本直达", "完成", cfg.PrimaryModel, "未检测到图片，跳过视觉候选链。", time.Now())
	}
	if err != nil {
		cfg.events.stage(event, "多模态预处理", "失败", "", err.Error(), time.Now())
		cfg.events.finish(event, err)
		return nil, err
	}
	body, err = prepareFinalTextBody(body, cfg, req.HostCallbackID, event)
	if err != nil {
		cfg.events.stage(event, "主上下文预算", "失败", cfg.PrimaryModel, err.Error(), time.Now())
		cfg.events.finish(event, err)
		return nil, err
	}
	primaryStarted := time.Now()
	cfg.events.stage(event, "交给文本模型链", "完成", cfg.PrimaryModel, "请求已完成附件处理与上下文预算检查；图片不会进入文本模型。", primaryStarted)
	response, usedModel, err := executeTextFallback(req.HostCallbackID, cfg, body, protocol, event)
	if err != nil {
		cfg.events.stage(event, "文本模型链返回", "失败", usedModel, err.Error(), primaryStarted)
		cfg.events.finish(event, err)
		return nil, err
	}
	cfg.events.stage(event, "文本模型链返回", "完成", usedModel, "已生成最终非流式回答。", primaryStarted)
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
	protocol, err := executorProtocol(req)
	if err != nil {
		return nil, err
	}
	cfg := currentConfig()
	event := cfg.events.begin(req.Model, cfg.PrimaryModel, true)
	go func() {
		started := time.Now()
		cfg.events.stage(event, "接收组合请求", "完成", req.Model, fmt.Sprintf("已识别流式 %s 请求，开始检查多模态内容。", protocol), started)
		body, images, err := preparePrimaryBody(req.OriginalRequest, protocol, cfg, req.HostCallbackID, event)
		cfg.events.setImageCount(event, images)
		if images == 0 {
			cfg.events.stage(event, "纯文本直达", "完成", cfg.PrimaryModel, "未检测到图片，跳过视觉候选链。", time.Now())
		}
		if err == nil {
			body, err = prepareFinalTextBody(body, cfg, req.HostCallbackID, event)
		}
		if err == nil {
			primaryStarted := time.Now()
			cfg.events.stage(event, "交给文本模型链", "完成", cfg.PrimaryModel, "请求已完成附件处理与上下文预算检查，开始透传输出流。", primaryStarted)
			var usedModel string
			usedModel, err = forwardTextFallbackStream(req.StreamID, req.HostCallbackID, cfg, body, protocol, event)
			if err != nil {
				cfg.events.stage(event, "文本流结束", "失败", usedModel, err.Error(), primaryStarted)
			} else {
				cfg.events.stage(event, "文本流结束", "完成", usedModel, "流式输出已完整透传。", primaryStarted)
			}
		} else {
			cfg.events.stage(event, "多模态预处理", "失败", "", err.Error(), time.Now())
		}
		cfg.events.finish(event, err)
		closePluginStream(req.StreamID, err)
	}()
	return okEnvelope(map[string]any{"headers": http.Header{"Content-Type": []string{"text/event-stream"}}})
}

func preparePrimaryBody(raw []byte, protocol string, cfg runtimeConfig, callbackID string, event *comboEvent) ([]byte, int, error) {
	if len(raw) == 0 {
		return nil, 0, fmt.Errorf("original %s request is missing", protocol)
	}
	return transformRequest(raw, protocol, cfg, func(asset visualAsset, contextText string) (string, error) {
		return describeImage(cfg, callbackID, asset, contextText, event)
	})
}
func describeImage(cfg runtimeConfig, callbackID string, asset visualAsset, contextText string, event *comboEvent) (string, error) {
	if err := validateAsset(asset.URL, cfg); err != nil {
		return "", err
	}
	key := visualCacheKey(cfg, asset, contextText)
	if key != "" {
		if cached, ok := cfg.cache.get(key); ok {
			cfg.events.stage(event, "读取视觉记忆缓存", "完成", "缓存", "同一图片命中本地内存缓存，未再次调用视觉模型。", time.Now())
			return cached, nil
		}
	}
	value, joined, err := cfg.cache.do(key, func() (string, error) {
		cfg.events.stage(event, "进入视觉候选链", "完成", "", fmt.Sprintf("仅携带当前用户附近文字（最多 %d token）；识别请求强制 low 思考。", cfg.VisionInputTokenBudget), time.Now())
		var lastErr error
		for _, candidate := range cfg.VisionModels {
			if !candidate.active() {
				cfg.events.stage(event, "视觉候选跳过", "跳过", candidate.Model, "该候选模型已在配置中停用。", time.Now())
				continue
			}
			projectedInput := estimateTokens(contextText) + cfg.VisionImageTokenReserve
			if projectedInput > candidate.ContextBudget || projectedInput+candidate.MaxOutputTokens > candidate.ContextLimit {
				lastErr = fmt.Errorf("vision model %s skipped: projected context exceeds %d", candidate.Model, candidate.ContextLimit)
				cfg.events.stage(event, "视觉上下文预检", "跳过", candidate.Model, fmt.Sprintf("预测输入 %d token，超过工作预算 %d 或总上限 %d，未发送请求。", projectedInput, candidate.ContextBudget, candidate.ContextLimit), time.Now())
				continue
			}
			candidateStarted := time.Now()
			request := makeVisionRequest(candidate.Model, cfg.VisionPrompt, contextText, asset.URL, candidate.MaxOutputTokens)
			cfg.events.stage(event, "视觉候选调用", "进行中", candidate.Model, fmt.Sprintf("启动 CPA Host 视觉流；取得 stream ID 后启用 %d 秒可取消预算，取消确认最多等待 %d 秒。", candidate.TimeoutSeconds, cfg.VisionCancelGraceSeconds), candidateStarted)
			description, err := hostExecuteVisionStreamWithTimeout(callbackID, lowThinkingModel(candidate.Model), request, candidate.TimeoutSeconds, cfg.VisionCancelGraceSeconds)
			if err != nil {
				lastErr = err
				if isVisionCancellationUnconfirmed(err) {
					cfg.events.stage(event, "视觉取消确认", "失败", candidate.Model, "超时后未能确认上游流已结束；为避免重叠调用和重复计费，已停止本次视觉回退："+err.Error(), candidateStarted)
					return "", err
				}
				detail := "调用失败，继续尝试下一个候选：" + err.Error()
				if isVisionStreamTimeout(err) {
					detail = "可取消识别阶段超时，已确认关闭上游流；安全尝试下一个候选。"
				}
				cfg.events.stage(event, "视觉候选调用", "失败", candidate.Model, detail, candidateStarted)
				continue
			}
			if description == "" {
				lastErr = fmt.Errorf("vision model %s returned no usable text", candidate.Model)
				cfg.events.stage(event, "视觉候选调用", "失败", candidate.Model, "返回为空，继续尝试下一个候选。", candidateStarted)
				continue
			}
			cfg.cache.set(key, "vision", description, cacheTTL(cfg))
			cfg.events.stage(event, "视觉识别完成", "完成", candidate.Model, "已提取视觉记忆：\n"+description, candidateStarted)
			cfg.events.stage(event, "注入视觉记忆", "完成", cfg.PrimaryModel, "原始图片片段已替换为上方视觉记忆文本，随后继续由主文本模型完成任务。", time.Now())
			return description, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no enabled visual model is configured")
		}
		return "", lastErr
	})
	if joined && err == nil {
		cfg.events.stage(event, "合并重复识图请求", "完成", "缓存", "相同图片正在识别，本请求复用了同一任务。", time.Now())
	}
	return value, err
}

func textModels(cfg runtimeConfig) []string {
	return uniqueModels(append([]string{cfg.PrimaryModel}, cfg.TextFallbackModels...))
}

func executeTextFallback(callbackID string, cfg runtimeConfig, body []byte, protocol string, event *comboEvent) (pluginapi.HostModelExecutionResponse, string, error) {
	var lastErr error
	models := textModels(cfg)
	for index, model := range models {
		started := time.Now()
		response, err := hostExecuteProtocol(callbackID, model, body, false, protocol)
		if err == nil {
			return response, model, nil
		}
		lastErr = err
		detail := err.Error()
		if index+1 < len(models) {
			detail += "；尝试下一个文本备用模型。"
		}
		cfg.events.stage(event, "文本候选调用", "失败", model, detail, started)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no text model is configured")
	}
	return pluginapi.HostModelExecutionResponse{}, models[len(models)-1], lastErr
}

func hostExecute(callbackID, model string, body []byte, stream bool) (pluginapi.HostModelExecutionResponse, error) {
	return hostExecuteProtocol(callbackID, model, body, stream, "openai")
}

func hostExecuteProtocol(callbackID, model string, body []byte, stream bool, protocol string) (pluginapi.HostModelExecutionResponse, error) {
	raw, err := callHost(pluginabi.MethodHostModelExecute, makeHostModelRequest(callbackID, protocol, model, body, stream))
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

func makeHostModelRequest(callbackID, protocol, model string, body []byte, stream bool) hostModelRequest {
	return hostModelRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: protocol,
			ExitProtocol:  protocol,
			Model:         model,
			Stream:        stream,
			Body:          body,
		},
		HostCallbackID: callbackID,
	}
}

// hostExecuteWithTimeout is retained for the history-compression path. The
// non-streaming Host ABI has no cancellation primitive, so this is only a
// latency annotation and must never be used for visual-model fallback.
func hostExecuteWithTimeout(callbackID, model string, body []byte, seconds int) (pluginapi.HostModelExecutionResponse, error) {
	started := time.Now()
	response, err := hostExecute(callbackID, model, body, false)
	if err != nil && seconds > 0 && time.Since(started) > time.Duration(seconds)*time.Second {
		return response, fmt.Errorf("model %s failed after exceeding the %ds soft latency budget: %w", model, seconds, err)
	}
	return response, err
}

func forwardTextFallbackStream(streamID, callbackID string, cfg runtimeConfig, body []byte, protocol string, event *comboEvent) (string, error) {
	models := textModels(cfg)
	var lastErr error
	for index, model := range models {
		started := time.Now()
		emitted, err := forwardPrimaryStream(streamID, callbackID, model, body, protocol)
		if err == nil {
			return model, nil
		}
		lastErr = err
		// Once bytes reached the client, switching models would duplicate or mix
		// two answers in one stream. Fallback is therefore safe only before the
		// first emitted chunk.
		if emitted {
			return model, err
		}
		detail := err.Error()
		if index+1 < len(models) {
			detail += "；尚未输出内容，安全切换到下一个文本备用模型。"
		}
		cfg.events.stage(event, "文本流候选", "失败", model, detail, started)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no text model is configured")
	}
	return models[len(models)-1], lastErr
}

func forwardPrimaryStream(streamID, callbackID, model string, body []byte, protocol string) (bool, error) {
	raw, err := callHost(pluginabi.MethodHostModelExecuteStream, makeHostModelRequest(callbackID, protocol, model, body, true))
	if err != nil {
		return false, err
	}
	var started pluginapi.HostModelStreamResponse
	if err := json.Unmarshal(raw, &started); err != nil {
		return false, err
	}
	if started.StatusCode >= 400 {
		return false, fmt.Errorf("text model %s returned HTTP %d", model, started.StatusCode)
	}
	if started.StreamID == "" {
		return false, fmt.Errorf("text model %s returned no stream id", model)
	}
	defer func() {
		_, _ = callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: started.StreamID})
	}()
	emitted := false
	for {
		chunkRaw, err := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: started.StreamID})
		if err != nil {
			return emitted, err
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if err := json.Unmarshal(chunkRaw, &chunk); err != nil {
			return emitted, err
		}
		if chunk.Error != "" {
			return emitted, fmt.Errorf("text stream error: %s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if _, err = callHost(pluginabi.MethodHostStreamEmit, streamEmitRequest{StreamID: streamID, Payload: chunk.Payload}); err != nil {
				return emitted, err
			}
			emitted = true
		}
		if chunk.Done {
			return emitted, nil
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
