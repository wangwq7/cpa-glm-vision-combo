# CPA GLM Vision Bridge

`v0.4.2` 是适用于 CLIProxyAPI v7（CPA）的原生视觉桥接插件。它让不支持图片的长上下文文本模型继续担任主模型：视觉模型只负责把图片转换成受控文本，最终理解、推理和回答仍由 GLM 等首选文本模型完成。

仓库包含完整 Go 源码、测试、中文管理页面、配置迁移脚本和 Linux amd64 构建脚本。

## 对外模型

**只暴露一个**对外虚拟模型名（默认 `glm-5.2-vision-combo`），由配置项 `combo_model` 决定。不再支持对外别名。

Agent、Claude Code、Codex 或其他 OpenAI/Claude 兼容客户端只需把 `model` 设置为该名称。插件会保持客户端协议：OpenAI 请求得到 OpenAI 响应，Claude `/v1/messages` 请求得到 Claude 响应；客户端不需要知道内部的文本模型和视觉模型名称。

```json
{
  "model": "glm-5.2-vision-combo",
  "messages": [
    { "role": "user", "content": "请分析这张截图" }
  ]
}
```

## 路由过程

```text
客户端调用 glm-5.2-vision-combo
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
- Claude `tool_result.content[]` 中的嵌套图片会按原 JSON 路径替换，保留 `tool_use_id` 和完整工具历史结构。
- 同一图片按“图片内容 + 附近真实用户任务”缓存；不同问题不会误用同一份任务导向的识别结果。
- 多图片可受控并发，单张相同图片使用 single-flight，避免同一进程内重复识别。
- 无关的后续文本轮只保留历史图片短摘要；用户明确追问旧图时才恢复缓存详情。
- 长会话达到阈值后自动压缩较早对话，并保留最近若干轮原文。
- 图片中的文字被标记为 `gateway-generated / untrusted`，表示它是外部不可信数据，主模型不得把图片内的指令当成系统指令执行。
- 所有图片替换后执行残留媒体检查；未知图片结构和 PDF 会在进入主文本模型前明确失败，不会静默透传。
- **文本链与视觉链禁止使用相同模型名**；冲突时配置校验失败，管理页保存会直接提示错误。

## 超时、取消与顺序 fallback

`vision_timeout_seconds` 是 CPA Host 返回视觉 `stream_id` 后的可取消识别预算。视觉调用改走 CPA Host 的流式接口：预算耗尽时插件调用 `host.model.stream_close`，并最多等待 `vision_cancel_grace_seconds` 确认该流已经结束；只有确认后才会尝试下一个视觉模型。

- 已建立的流在预算内完成：聚合视觉文本并继续交给主文本模型。
- 已建立的流在预算内停滞或未完成：关闭并确认该流，再顺序尝试下一个候选。
- 任一候选失败、空结果或上下文预检失败：不并发重试，继续下一个候选。
- 若关闭后未获得 CPA Host 的结束确认：本次视觉请求直接失败，不启动备用模型，避免任何重叠调用或重复扣费。

CPA 当前 ABI 的边界仍需明确：它只有在自身收到第一个有效流数据后才把流 ID 交给插件。因此“首包前完全卡住”的调用不能在第 N 秒由插件立即中止，插件也不会在这一阶段抢跑备用模型。这样不会为了抢时间并发发出多个视觉请求，也不会重演请求风暴。

如果必须实现“第 20 秒无论是否首包都立即停止上游”，需要 CPA 未来提供可取消的首包前调用句柄或带 deadline 的 Host API；这不是 JS Handler 或纯插件脚本能够安全补齐的能力。

## 推荐配置

```yaml
plugins:
  enabled: true
  configs:
    glm-vision-combo:
      enabled: true
      priority: 100

      # 唯一对外模型名
      combo_model: glm-5.2-vision-combo

      primary_model: glm-5.2
      primary_context_tokens: 1048576
      primary_context_budget_tokens: 930000
      # 文本备用链：不要与下方视觉模型重复
      text_fallback_models: []

      vision_primary_model: gpt-5.4-mini
      vision_backup_model_1: gpt-5.6-luna
      vision_backup_model_2: claude-sonnet-4-6
      vision_backup_model_3: gemini-3.1-flash-lite
      vision_context_limit: 262144
      vision_input_token_budget: 1200
      vision_output_tokens: 4000
      vision_timeout_seconds: 20
      vision_cancel_grace_seconds: 15

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
| `combo_model` | 对客户端暴露的**唯一**虚拟模型名 | 默认 `glm-5.2-vision-combo` |
| `primary_model` | 最终回答的首选文本模型 | 使用长上下文、推理稳定的模型 |
| `primary_context_tokens` | 文本模型理论上下文上限 | GLM-5.2 为 1M 时填 `1048576` |
| `primary_context_budget_tokens` | 实际工作安全线 | 必须低于理论上限，预留输出和协议开销 |
| `text_fallback_models` | 文本模型首包前失败后的备用链 | 可留空；**禁止**与视觉链模型重复 |
| `vision_primary_model` | 第一视觉模型 | 优先低延迟、OCR 稳定的模型 |
| `vision_backup_model_1..3` | 顺序视觉备用链 | 可留空；最多四个视觉候选；**禁止**与文本链重复 |
| `vision_context_limit` | 视觉模型上下文上限 | 256K 模型填 `262144` |
| `vision_input_token_budget` | 发给视觉模型的附近文字预算 | 普通截图建议 800–2000 |
| `vision_output_tokens` | 单张图片识别输出上限 | 普通截图 1K–4K，复杂表格可提高 |
| `vision_timeout_seconds` | 已建立视觉流的可取消预算 | 建议 20–30 秒；从 CPA 返回 `stream_id` 后开始计时 |
| `vision_cancel_grace_seconds` | 取消确认等待上限 | 默认 15 秒；未确认流结束时停止本次 fallback，避免重叠请求 |
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

> **重要：** 若某个模型同时出现在 `primary_model` / `text_fallback_models` 与视觉候选中，配置校验会失败，插件无法注册。管理页保存时也会前端拦截并提示冲突模型名。

## 管理页面

插件注册了 CPA Management API 页面，提供：

- 全中文配置说明。
- 从 CPA 当前可用模型中下拉选择文本和视觉模型。
- 当前组合模型的路由流程预览。
- 缓存命中、视觉 fallback、压缩、文本 fallback 和阶段耗时事件。
- 保存前校验：文本链与视觉链模型不得重复；对外模型名必填。

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
dist/glm-vision-combo_0.4.2_linux_amd64.zip
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
4. 确认 `config.yaml` 中 `combo_model` 为唯一对外名，且文本链/视觉链无重复模型。
5. 重启 CPA 或重新加载插件。
6. 在日志中确认 `GLM Vision Bridge version=0.4.2`。
7. 请求 `/v1/models`，确认**仅**存在配置的 `combo_model`。
8. 依次测试纯文本、首次截图、同图缓存、无关文本和追问旧图。

从旧版配置升级时可参考：

```bash
python3 migrate-config-v030.py /path/to/config.yaml
```

迁移前务必保留配置备份，并检查脚本输出的 diff。旧版 `combo_aliases` 会被忽略；客户端请统一改用 `combo_model`。

本插件为**本地/GitHub 手动安装**，不在官方插件商店 registry 中。失效时请修复配置并重启 CPA，不要从商店搜索重装。

## 回退

如果新版插件加载失败：

1. 停止或重载 CPA 插件。
2. 用升级前保存的 `.bak` 恢复 `glm-vision-combo.so`。
3. 恢复对应版本的配置文件。
4. 重启后检查插件版本和 `/v1/models`。

缓存文件只包含识别文本，通常可跨 v0.3.x/v0.4.x 保留；若发现格式或权限异常，可先备份后移走缓存文件，让插件自动重建。

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
- CPA 插件可在 Host 流建立后用 `host.model.stream_close` 取消上游，并等待结束确认；首包前仍受 CPA ABI 限制，不能在精确的 deadline 立刻中止。

## 已知限制

- 当前版本不处理 PDF 专用解析；PDF 会被明确拒绝，不会送入只支持图片的视觉链。PDF 策略应根据文本型、扫描型和页数另行设计。
- CPA Host 尚未向插件暴露首包前的可取消调用句柄；因此无法纯靠插件实现精确的首包硬取消。
- CPA v7.2.71 的 `/v1/models` 可能不展示这个虚拟组合模型，但使用完整模型名发起请求仍会由 Model Router 正常接管。
- 进程重启后事件时间线清空；视觉文本缓存可按配置持久化。
- 图片识别结果属于模型生成内容，可能有 OCR 错误；最终模型应保留不确定性说明。

## 版本说明

### v0.4.2

- 新增 Claude `/v1/messages` 输入输出协议，并支持 `tool_result.content[]` 中的 base64/URL 图片。
- 保留 `tool_use_id` 与工具历史结构，从附件位置向前提取真实用户任务，忽略工具输出中的伪任务文字。
- 视觉缓存键加入附近用户任务；相同图片用于不同问题时会分别识别。
- 媒体预检与替换后残留检查改为失败关闭，明确拒绝 PDF 和未知媒体结构。
- 视觉流聚合兼容无尾换行的 Host chunk，以及 OpenAI Chat、OpenAI Responses、Anthropic、Gemini/Antigravity 事件格式。
- OpenAI 与 Claude 的最终非流式/流式回答均保持原始客户端协议。

### v0.4.1

- 在现有 v0.4.0 的可取消视觉流基础上，保留“取消确认前绝不启动备用模型”的保护。
- 拒绝把组合模型自身或别名配置为视觉候选，防止递归路由。
- 不修改 CPA 源码、CPA 配置格式或 vendor；旧配置会自动补齐 `vision_cancel_grace_seconds: 15`。

### v0.4.0

- 视觉候选改为 CPA Host 流式调用，并聚合 OpenAI SSE 视觉文本。
- 流建立后，预算耗尽会调用 `host.model.stream_close`，确认结束后才按顺序 fallback。
- 新增流式 SSE 聚合和取消确认测试；不修改 CPA 源码或 vendor。
- 管理页将超时说明改为真实的总预算与 ABI 边界。

### v0.3.2

- 只对外暴露一个 `combo_model`；忽略历史 `combo_aliases`。
- 管理页移除“对外别名”输入框。
- 管理页保存前校验：文本链与视觉链模型不得重复，冲突时直接提示并拒绝保存。
- 补充冲突场景的后端测试与 README 说明。

### v0.3.1

- 以已经过完整流程验证的 v0.3 路由逻辑为准。
- 补齐安装、升级、回退、参数、性能、缓存和软超时文档。
- 不引入实验性流式聚合或并行 fallback，不改变稳定请求路径。

许可证及上游依赖许可见仓库和 `vendor` 目录中的对应文件。
