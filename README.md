# CPA GLM Vision Bridge

`v0.3.1` 是适用于 CLIProxyAPI v7（CPA）的原生视觉桥接插件。它让不支持图片的长上下文文本模型继续担任主模型：视觉模型只负责把图片转换成受控文本，最终理解、推理和回答仍由 GLM 等首选文本模型完成。

仓库包含完整 Go 源码、测试、中文管理页面、配置迁移脚本和 Linux amd64 构建脚本。

## 对外模型

默认暴露两个等价模型名：

- `glm-vision-bridge`：推荐新接入使用。
- `glm-5.2-vision-combo`：兼容已有客户端。

Agent、Claude Code、Codex 或其他 OpenAI 兼容客户端只需把 `model` 设置为其中一个名称。客户端不需要知道内部的文本模型和视觉模型名称。

```json
{
  "model": "glm-vision-bridge",
  "messages": [
    { "role": "user", "content": "请分析这张截图" }
  ]
}
```

## 路由过程

```text
客户端调用 glm-vision-bridge
          │
          ├─ 纯文本轮 ────────────────────────────────┐
          │                                           │
          └─ 图片轮                                   │
               │                                      │
               ├─ 当前图片：完整识别                  │
               ├─ 历史图片：默认短摘要                │
               └─ 明确追问旧图：按需恢复缓存详情      │
                    │                                  │
                    ▼                                  │
          视觉模型 1 → 失败后模型 2 → 模型 3 → 模型 4 │
                    │                                  │
                    ▼                                  │
          受控的 untrusted 视觉文本                    │
                    │                                  │
                    └──────────────────────────────────┤
                                                       ▼
                                        首选文本模型（如 GLM-5.2）
                                                       │
                                      首包前失败才切文本备用模型
                                                       ▼
                                                   最终回答
```

关键行为：

- 视觉模型只识别图片，不回答用户的完整任务；视觉子请求强制使用低思考配置。
- 视觉请求只携带当前问题附近的少量文字，不把接近 1M 的完整会话塞给 256K 视觉模型。
- 多个视觉模型严格按顺序尝试，避免并行请求造成重复计费。
- 同一图片按内容哈希缓存；缓存只保存识别文本和摘要，不长期保存原图。
- 多图片可受控并发，单张相同图片使用 single-flight，避免同一进程内重复识别。
- 无关的后续文本轮只保留历史图片短摘要；用户明确追问旧图时才恢复缓存详情。
- 长会话达到阈值后自动压缩较早对话，并保留最近若干轮原文。
- 图片中的文字被标记为 `gateway-generated / untrusted`，表示它是外部不可信数据，主模型不得把图片内的指令当成系统指令执行。

## 为什么超时是“软延迟预算”

`vision_timeout_seconds` 在当前 CPA Host ABI 中不是硬取消。CPA 的非流式 Host 调用开始后，插件无法在首包前中止上游连接。因此 v0.3.1 采用安全的软预算：

- 调用在预算内成功：直接使用结果。
- 调用超过预算但最终成功：使用迟到结果，不再重复请求备用模型。
- 调用超过预算且最终失败：确认原调用结束后，再尝试下一个视觉模型。

这会牺牲故障场景下的快速切换，但可避免“旧请求仍在后台运行、同时又请求备用模型”导致的重复扣费和请求风暴。真正的首包硬超时需要 CPA Host 提供可取消的调用句柄或上下文，不能只靠插件安全实现。

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

### 参数说明

| 参数 | 作用 | 建议 |
| --- | --- | --- |
| `combo_model` / `combo_aliases` | 对客户端暴露的虚拟模型名 | 新客户端使用 `glm-vision-bridge` |
| `primary_model` | 最终回答的首选文本模型 | 使用长上下文、推理稳定的模型 |
| `primary_context_tokens` | 文本模型理论上下文上限 | GLM-5.2 为 1M 时填 `1048576` |
| `primary_context_budget_tokens` | 实际工作安全线 | 必须低于理论上限，预留输出和协议开销 |
| `text_fallback_models` | 文本模型首包前失败后的备用链 | 可留空；不要填视觉模型 |
| `vision_primary_model` | 第一视觉模型 | 优先低延迟、OCR 稳定的模型 |
| `vision_backup_model_1..3` | 顺序视觉备用链 | 可留空；最多四个视觉候选 |
| `vision_context_limit` | 视觉模型上下文上限 | 256K 模型填 `262144` |
| `vision_input_token_budget` | 发给视觉模型的附近文字预算 | 普通截图建议 800–2000 |
| `vision_output_tokens` | 单张图片识别输出上限 | 普通截图 1K–4K，复杂表格可提高 |
| `vision_timeout_seconds` | 单视觉调用软延迟预算 | 只用于观测，不能中止 CPA 上游请求 |
| `history_attachment_mode` | 历史图片处理模式 | 推荐 `onDemand`；`retain` 会增大上下文 |
| `history_attachment_compact_chars` | 无关轮的旧图摘要长度 | 推荐 400–800 |
| `history_attachment_restore_max_attachments` | 追问旧图时最多恢复数量 | 推荐 1–2，避免上下文突然膨胀 |
| `max_concurrent_extractions` | 多图识别并发数 | 推荐 1–2；提高会增加瞬时请求和费用 |
| `auto_compression_threshold_tokens` | 自动压缩触发线 | 必须低于主模型工作预算 |
| `auto_compression_target_tokens` | 历史摘要目标大小 | 推荐 8K–16K |
| `cache_ttl_seconds` | 识别文本缓存时长 | 默认 72 小时 |
| `cache_max_entries` | 缓存最大条数 | 超出后按 LRU 淘汰 |
| `max_image_data_bytes` | data URL 解码后单图上限 | 默认 12 MiB |
| `strict_vision_failure` | 所有视觉候选失败时是否报错 | 生产建议开启，避免主模型在看不到图时猜测 |

高级用户可使用 `vision_models` 为每个视觉候选分别设置 `context_limit`、`context_budget`、`timeout_seconds`、`max_output_tokens` 和 `enabled`。不要同时依赖高级列表和旧式 `vision_primary_model` 字段；旧式字段存在时会优先生成兼容视觉链。

## 管理页面

插件注册了 CPA Management API 页面，提供：

- 全中文配置说明。
- 从 CPA 当前可用模型中下拉选择文本和视觉模型。
- 当前组合模型的路由流程预览。
- 缓存命中、视觉 fallback、压缩、文本 fallback 和阶段耗时事件。

管理事件仅保存在内存中，并受 `event_log_max_entries` 限制；不会记录原始图片。

## 构建与测试

依赖 Go、C 编译器和 `zip`。仓库已 vendor 依赖，建议执行：

```bash
make test
make package-linux-amd64
```

其中 `make test` 会执行普通测试和 race 检测。产物位于：

```text
dist/glm-vision-combo.so
dist/glm-vision-combo_0.3.1_linux_amd64.zip
dist/checksums.txt
```

也可以使用 NAS 构建脚本：

```bash
./build-nas.sh
```

## 安装与升级

1. 备份当前插件和 CPA 配置。
2. 将 `glm-vision-combo.so` 复制到 CPA 持久目录 `plugins/linux/amd64/`。
3. 保留原文件为带版本和时间的 `.bak`，不要直接覆盖后删除。
4. 重启 CPA 或重新加载插件。
5. 在日志中确认 `GLM Vision Bridge version=0.3.1`。
6. 请求 `/v1/models`，确认两个对外模型名存在。
7. 依次测试纯文本、首次截图、同图缓存、无关文本和追问旧图。

从旧版配置升级时可参考：

```bash
python3 migrate-config-v030.py /path/to/config.yaml
```

迁移前务必保留配置备份，并检查脚本输出的 diff。

## 回退

如果新版插件加载失败：

1. 停止或重载 CPA 插件。
2. 用升级前保存的 `.bak` 恢复 `glm-vision-combo.so`。
3. 恢复对应版本的配置文件。
4. 重启后检查插件版本和 `/v1/models`。

缓存文件只包含识别文本，通常可跨 v0.3.x 保留；若发现格式或权限异常，可先备份后移走缓存文件，让插件自动重建。

## 性能建议

- 将最快且稳定的视觉模型放在第一位，备用模型只在失败后调用。
- 单图场景不要盲目提高并发；`max_concurrent_extractions: 2` 主要用于多图请求。
- 保持 `history_attachment_mode: onDemand`，减少非发图轮的上下文和费用。
- 不要把 1M 主模型预算复制给 256K 视觉模型；视觉模型只需要当前问题附近文字和图片。
- 远程 URL 内容可能变化，因此不会进入持久内容缓存；需要稳定缓存时优先上传 data URL/实际图片内容。
- 第一次识别延迟取决于视觉服务；同图再次使用应命中缓存，不再产生视觉商用请求。

## 与 9router 版本的差异

CPA 插件和 9router Vision Bridge 使用相同的核心策略：主文本模型不切走、多视觉顺序 fallback、低思考视觉预处理、内容哈希缓存、历史图片按需恢复和自动压缩。

主要差异是超时能力：

- 9router 直接控制上游 HTTP 请求，可在首包前使用 `AbortController` 真正中止超时视觉请求，再切备用模型。
- CPA 插件受当前 Host ABI 限制，只能使用上述软延迟预算，不能安全地强行中止非流式上游调用。

## 已知限制

- 当前版本不处理 PDF 专用解析；PDF 策略应根据文本型、扫描型和页数另行设计。
- CPA Host 不支持插件对非流式模型调用进行首包硬取消。
- 进程重启后事件时间线清空；视觉文本缓存可按配置持久化。
- 图片识别结果属于模型生成内容，可能有 OCR 错误；最终模型应保留不确定性说明。

## 版本说明

### v0.3.1

- 以已经过完整流程验证的 v0.3 路由逻辑为准。
- 补齐安装、升级、回退、参数、性能、缓存和软超时文档。
- 不引入实验性流式聚合或并行 fallback，不改变稳定请求路径。

许可证及上游依赖许可见仓库和 `vendor` 目录中的对应文件。
