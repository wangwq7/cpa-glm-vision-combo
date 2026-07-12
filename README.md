# CPA GLM Vision Bridge

CLIProxyAPI v7 原生插件。它暴露一个可供 Agent 直接调用的组合模型：图片先经过有顺序的视觉候选链转成文本，最终任务仍由 GLM 等首选文本模型完成。

默认同时暴露：

- `glm-5.2-vision-combo`：兼容旧调用
- `glm-vision-bridge`：简洁别名

## v0.3.0 路由逻辑

```text
OpenAI 请求
→ 区分当前图片与历史图片
→ 当前图片完整识别；历史图片默认短摘要，明确引用时恢复详情
→ 每张图片按视觉候选顺序 fallback，识别请求强制 low
→ 内容哈希缓存 + single-flight + 多图并发
→ 以 gateway-generated / untrusted context 包装识别文本
→ 超过阈值时自动压缩较早对话
→ 首选文本模型完成任务
→ 尚未输出内容时可安全切换文本备用模型
```

缓存只保存视觉识别文本和历史摘要，不保存原始图片。data URL 使用解码后的真实字节数校验；远程 URL 因内容可能变化，不进入持久缓存。

## 推荐配置

```yaml
plugins:
  enabled: true
  configs:
    glm-vision-combo:
      enabled: true
      priority: 100
      combo_model: glm-5.2-vision-combo
      combo_aliases:
        - glm-vision-bridge

      primary_model: glm-5.2
      primary_context_tokens: 1048576
      primary_context_budget_tokens: 930000
      text_fallback_models: []

      vision_primary_model: gpt-5.4-mini
      vision_backup_model_1: gpt-5.6-luna
      vision_backup_model_2: claude-sonnet-4-6
      vision_backup_model_3: gemini-3.1-flash-lite
      vision_context_limit: 262144
      vision_input_token_budget: 1200
      vision_output_tokens: 4000
      vision_timeout_seconds: 20

      history_attachment_mode: onDemand
      history_attachment_compact_chars: 600
      history_attachment_restore_max_attachments: 2
      max_images_per_request: 8
      max_concurrent_extractions: 2

      auto_compression_enabled: true
      auto_compression_threshold_tokens: 720000
      auto_compression_target_tokens: 12000
      auto_compression_keep_recent_turns: 8
      auto_compression_model: ""

      cache_ttl_seconds: 259200
      cache_max_entries: 2000
      cache_path: /CLIProxyAPI/plugins/data/glm-vision-combo-cache.json
      max_image_data_bytes: 12582912
      allow_remote_image_urls: true
      strict_vision_failure: true
      event_log_max_entries: 100
```

管理页提供完整中文说明、运行时模型下拉框、实时路由预览和真实请求事件时间线。

`vision_timeout_seconds` 在 CPA 插件中是软延迟预算。当前 Host ABI 无法取消已经开始的非流式模型调用；插件会采用成功的迟到结果，只有 Host 真正返回失败后才进入下一视觉候选，从而避免遗留请求、重复计费和并发重试风暴。

## 构建

```bash
make test
make package-linux-amd64
```

将生成的 `glm-vision-combo.so` 安装到 CPA 持久目录 `plugins/linux/amd64/`，然后重启或重新加载插件。
