# CPA GLM Vision Bridge

`v0.7` 是适用于 CLIProxyAPI v7（CPA）的原生视觉桥接插件。它让不支持图片的长上下文文本模型继续担任主模型：视觉模型只负责把图片转换成受控文本，最终理解、推理和回答仍由 GLM 等首选文本模型完成。

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
               ├─ 历史图片：默认固定归档标记          │
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
- 图片已转换为视觉记忆后，主文本模型不再获得 `view_image` 工具，避免 Agent 在同一轮反复看同一张图；其他工具保持可用。
- 无关的后续文本轮把未引用旧图合并为一条固定归档标记，不逐张解码、哈希或注入长摘要；用户明确追问旧图时才恢复最近的有限张图片。
- 自动历史压缩只在安全语义边界执行：OpenAI Responses、OpenAI Chat 和 Claude Messages 的工具调用与结果不会被拆到摘要边界两侧；未完成的工具事务继续留在原始后缀。
- 长会话首次达到阈值时建立可持久复用的历史摘要检查点；后续追加少量对话直接复用，只有新增尾部再次逼近预算时才按完整语义轮次和工具事务增量更新一次。
- 纯文本大请求使用单遍 JSON/媒体扫描，不再先构造完整动态对象；不会向 GLM、GPT、Claude、Gemini、Grok 等上游注入任何厂商私有缓存字段。
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
      text_fallback_models:
        - gpt-5.5
        - gpt-5.6-sol

      vision_primary_model: gemini-3.1-flash-lite
      vision_backup_model_1: gpt-5.6-terra
      vision_backup_model_2: grok-4.5
      vision_backup_model_3: claude-sonnet-4-6
      vision_context_limit: 262144
      vision_input_token_budget: 1200
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

### 生产实测依据

2026-07-15 使用同一张包含中文/英文标题、状态卡、四行数值表格、控件选中状态、工单号和 Go 代码的截图测试生产 CPA 模型。标准图和 50% 缩略图中四个候选均完整命中 23/23 个核验点；精确复现插件 `(low)` 调用方式后，`gemini-3.1-flash-lite`、`gpt-5.6-terra`、`grok-4.5`、`claude-sonnet-4-6` 的完成时间分别约为 6.9、11.2、11.6、13.7 秒。

因此默认生产链采用 `Gemini Flash Lite → GPT Terra → Grok → Claude Sonnet`。四个候选均在 20 秒内完成，保留 `vision_timeout_seconds: 20`；`vision_cancel_grace_seconds: 15` 只在取消异常流时生效，不增加正常请求延迟。

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
| `vision_timeout_seconds` | 已建立视觉流的可取消预算 | 建议 20–30 秒；从 CPA 返回 `stream_id` 后开始计时 |
| `vision_cancel_grace_seconds` | 取消确认等待上限 | 默认 15 秒；未确认流结束时停止本次 fallback，避免重叠请求 |
| `history_attachment_mode` | 历史图片处理模式 | 推荐 `onDemand`；`retain` 会增大上下文 |
| `history_attachment_compact_chars` | 无关轮的旧图归档标记最大长度 | 默认值可保留；标记本身为固定短文本 |
| `history_attachment_restore_max_attachments` | 追问旧图时最多恢复数量 | 推荐 1–2，避免上下文突然膨胀 |
| `max_concurrent_extractions` | 多图识别并发数 | 推荐 1–2；提高会增加瞬时请求和费用 |
| `auto_compression_threshold_tokens` | 自动压缩触发线 | 必须低于主模型工作预算 |
| `auto_compression_target_tokens` | 历史摘要目标大小 | 推荐 8K–16K |
| `cache_ttl_seconds` | 识别文本缓存时长 | 默认 72 小时 |
| `cache_max_entries` | 缓存最大条数 | 超出后按 LRU 淘汰 |
| `max_image_data_bytes` | data URL 解码后单图上限 | 默认 12 MiB |
| `strict_vision_failure` | 所有视觉候选失败时是否报错 | 生产建议开启，避免主模型在看不到图时猜测 |

CPA Manager Plus 参数页使用 `vision_primary_model` 与三个备用字段维护四级视觉链，避免和高级配置同时出现。高级用户仍可直接在 YAML 中使用 `vision_models`，为每个视觉候选分别设置 `context_limit`、`context_budget`、`timeout_seconds` 和 `enabled`；不要同时依赖两套字段，旧式字段存在时会优先生成兼容视觉链。

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
dist/glm-vision-combo_0.5_linux_amd64.zip
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
6. 在日志中确认 `GLM Vision Bridge version=0.7`。
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
- 模型切换后的第一轮会丢失原供应商的前缀缓存；v0.7 不再通过独立删除工具轨迹来提速，历史压缩仅在达到阈值且位于安全语义边界时执行。
- 首次超长会话会创建摘要检查点；之后正常追加问答应出现“复用历史压缩检查点”，不再每轮调用压缩模型。
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

### v0.7

- 自动历史压缩改为优先在真实用户轮次起点和完整工具事务结束点选择摘要边界，不再按普通 JSON 消息条数直接切割。
- OpenAI Responses、OpenAI Chat 与 Claude Messages 的工具调用/结果在压缩后仍保持成对；未完成的工具调用继续保留在原始后缀，不进入摘要。
- 长单轮 Agent 任务即使没有新的 user message，也可以在完整工具事务之间安全建立或更新检查点。
- 历史检查点和摘要缓存升级到新的命名空间，避免复用旧版本可能生成的不安全边界。
- 本版本不处理首次切换到 GLM 时因上游前缀缓存失效造成的首轮延迟，也不改变模型自身的工具调用策略。

### v0.5.1

- 移除低于上下文压缩阈值时无条件执行的“旧工具轨迹归档”。OpenAI Chat、Responses 与 Claude Messages 的工具调用和工具结果现在完整透传，不再因已配对而被独立删除。
- 修复长工具任务在第 6 轮后只剩最近 1–2 对工具状态、导致模型反复重新调查的问题。自动历史压缩仅在达到配置阈值时运行，继续使用可复用摘要检查点。
- 新增三协议回归测试，确保包含大体积工具结果的正常请求在压缩阈值以下保持字节级不变。

### v0.5

- `/v1/responses` 的字符串型 `input` 会在插件内部规范化为等价的标准 user message 数组，避免首选 GLM 走 CPA Claude executor 时得到空 `messages`；数组型输入和其他请求参数保持不变。
- 最终文本请求移除客户端顶层 `max_tokens`，但保留客户端思考配置以及工具 Schema 等嵌套同名字段；视觉子请求继续固定 low 思考，并且不设置输出 token 上限。
- 视觉流补齐明确截断、空输出、取消确认和文本备用模型保护，未确认上游结束时不会并发启动 fallback。
- CPA Manager Plus 参数页同步可取消识别超时、取消确认等待和摘要目标的真实含义；通用参数页不再同时展示会覆盖四级视觉链的高级 `vision_models`。
- 修复 `/v1/responses` 的真实协议名映射，插件现在声明并处理 CPA 使用的 `openai-response`，不再绕过桥接后报 `auth_not_found`。

### v0.4.7

- 新增跨 OpenAI Chat、OpenAI Responses 和 Claude Messages 的旧工具轨迹本地归档：只移除保护窗口之外且完整配对的调用/结果，混合消息中的普通文本继续保留。
- 归档只在至少节省 4 KiB 时采用；无工具历史走快速扫描并原样返回，小轨迹、未配对轨迹和近期工具状态不会被修改。
- 管理事件新增“旧工具轨迹归档”和“文本上下文预检”，可直接核对归档前后 token/字节量及是否需要后续摘要模型。
- 纯文本请求改用单遍、低分配 JSON 媒体扫描；数 MB 的无图会话不再为了确认“没有图片”反序列化整棵对象。
- data URL 图片校验改为流式 base64 解码计数，不再分配一份完整解码图片；视觉缓存键改为增量哈希，不再拼接包含整张图片的临时大字符串。
- 图片替换后的残留媒体检查从两次完整树遍历合并为一次。
- 保持多厂商中立：不注入 `prompt_cache_key`、`prompt_cache_options`、`prompt_cache_retention`、`previous_response_id` 等 OpenAI 私有字段，GLM、GPT、Claude、Gemini、Grok 请求字段保持客户端原样。
- 本地基准中，约 2 MB 纯文本历史的预处理内存从约 2.13 MB / 725 次分配降至约 16–128 B / 1–2 次分配，稳定耗时约从 9 ms 降至 5–7 ms。

### v0.4.6

- 长会话改为持久的历史前缀检查点：首次超阈值只压缩一次，后续相同前缀和少量新增消息直接复用摘要。
- 新增历史再次逼近主模型预算时，仅使用“旧检查点摘要 + 新增历史”更新一次，不再从保留 8 轮反复重压到 1 轮。
- 未引用的历史图片合并为一条固定归档标记，不再逐张计算附近上下文、哈希完整 data URL、读取视觉缓存或 base64 解码。
- 当前轮图片和明确恢复的旧图片仍在任何视觉调用前严格校验；PDF 和未知媒体继续失败关闭。
- 管理事件新增“创建历史压缩检查点”“复用历史压缩检查点”“更新历史压缩检查点”，便于直接确认延迟路径。

### v0.4.5

- 修复 Agent 图片轮重复进入：视觉记忆生成后，从主文本模型工具列表中移除 `view_image`，阻止模型再次请求查看同一附件。
- 保留终端、文件、搜索、图片生成等其他工具；纯文本请求不受影响。
- 同时兼容 OpenAI Chat、OpenAI Responses 和 Claude Messages 的工具定义与 `tool_choice` 结构。

### v0.4.4

- 用生产 CPA 的标准截图和 50% 缩略图实测视觉候选，默认链调整为 `Gemini Flash Lite → GPT Terra → Grok → Claude Sonnet`。
- 视觉提示改为逐字保留时间戳、标识符、单位、符号、小数、表格单元格和代码，禁止跨界面区域合并数值。
- 视觉子请求固定使用高细节图片输入，避免缩略采样遗漏状态卡、小字号和密集表格。
- 缓存键加入视觉模型链和输出预算，升级或调序后不会继续复用旧模型生成的图片描述。
- 源码默认参数、管理页说明、推荐 YAML 和生产配置统一；`glm-5.2` 仍是最终回答者。

### v0.4.3

- 修正管理页错误显示为 `v0.4.1.2` 的版本文本。
- 管理页显式展示并保存 `vision_cancel_grace_seconds`，不再依赖旧版软延迟字段。
- 参数说明同步 OpenAI/Claude 自动协议适配、任务感知缓存和媒体失败关闭行为。

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
