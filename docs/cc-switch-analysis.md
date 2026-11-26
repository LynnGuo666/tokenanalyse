# CC Switch 源码分析与 Token Mirror 设计

> 分析对象：`farion1231/cc-switch`
> 本地源码：`/root/Code/vendor/cc-switch`
> 分析时源码版本：`v4.0.43`（commit `754fed72d428c66c1e6ed45f38601ac25ded90c6`）
> 已安装版本：`v4.0.42`
> License：MIT，Copyright (c) 2025 Jason Young

## 1. CC Switch 完整功能地图

CC Switch 是一个 Tauri 2 桌面应用，核心目标是把多个 AI 编程 CLI 的配置、供应商、扩展和会话集中管理。

### 1.1 工具接入

支持的工具包括：

- Claude Code
- Claude Desktop
- Codex
- Gemini CLI
- Grok Build
- OpenCode
- OpenClaw
- Hermes Agent
- Pi
- MiniMax Code
- Kimi Code
- Oh My Pi
- GitHub Copilot CLI
- CodeBuddy
- DeepSeek Harness（源码中已有适配器）

不同工具支持的能力不同：供应商切换/共存、本地路由、聚合、托盘切换、MCP、Skills、提示词、会话阅读、用量统计。

### 1.2 供应商与授权

- 90+ 供应商预设
- 自定义供应商
- 只修改连接字段，保留原配置中的插件、Hook、注释等
- 供应商拖拽排序、导入导出
- Claude Desktop 第三方接入与模型映射
- 通用供应商同步到 Claude Code / Codex / Gemini CLI
- OAuth / 订阅授权中心：GitHub Copilot、ChatGPT、xAI、Kimi Code 等
- 额度与余额查询：官方订阅、Coding Plan、账户余额、自定义用量脚本

### 1.3 本地路由与聚合

- Anthropic Messages、OpenAI Chat Completions、OpenAI Responses、Gemini Native 之间的请求/响应转换
- 每个工具独立启用本地路由
- 自动故障转移、熔断器、供应商健康检查
- 请求整流器：Thinking 签名、图片降级、协议兼容处理
- 聚合模式：把多家供应商的模型合并到一个模型列表，按模型路由
- 路由状态、监听地址、超时、重试、隐私过滤等设置

### 1.4 扩展管理

- MCP 服务器统一管理，按工具启用/停用
- 提示词库与 Markdown 编辑器
- Skills 搜索、安装、更新、同步
- 项目 Profiles：整套切换供应商、MCP、Skills、提示词
- Deep Link 导入供应商、MCP、提示词、Skills

### 1.5 会话与工作区

- 会话列表、搜索、分组
- 会话阅读页：正文、思考、工具调用、工具结果、图片
- 每轮摘要、失败步骤标记
- 会话内查找、导出 Markdown、复制恢复命令
- OpenClaw 工作区文件编辑
- Hermes 记忆管理

### 1.6 用量与成本

- 用量仪表盘
- 请求数、Token、缓存命中率、输出速度、估算费用
- 趋势图、全年热力图
- 请求日志与详情
- 按应用、供应商、模型、日期筛选
- 自定义模型定价
- models.dev 定价导入与自动同步
- USD/CNY 等显示汇率

### 1.7 系统与平台

- WebDAV / S3 配置快照同步
- 配置目录放入 Dropbox / OneDrive / iCloud
- CLI 工具安装、升级、重复安装诊断
- 系统托盘、开机自启、自动更新
- 原子写入、自动备份
- 深色 / 浅色 / 跟随系统
- 多语言

## 2. Token 统计架构

### 2.1 两条数据来源

```text
本地代理请求日志 ──┐
                  ├─> 统一 TokenUsage / 费用计算 ─> proxy_request_logs
CLI 本地会话日志 ──┘                                  │
                                                     ├─> usage_daily_rollups
                                                     └─> 查询 API / 桌面 UI
```

1. **本地代理日志**
   - 请求经过 CC Switch 本地路由时记录。
   - 有 provider、请求模型、返回模型、状态码、延迟、首 Token 时间、流式标记等。
   - 需要开启本地路由和请求用量记录。

2. **CLI 会话日志**
   - 不开本地路由也能统计。
   - 启动扫描一次，后台默认每 10 分钟扫描；打开用量页时立即扫描。
   - 会话日志通常无法可靠还原具体供应商，因此显示为 `{应用} · 会话日志`。

### 2.2 核心源码地图

| 文件 | 行数 | 职责 |
|---|---:|---|
| `src-tauri/src/proxy/usage/parser.rs` | 1446 | 解析 Claude / OpenAI / Codex / Gemini 响应与流式事件中的 TokenUsage |
| `src-tauri/src/proxy/usage/calculator.rs` | 299 | Decimal 高精度费用计算，处理缓存语义与 1 小时缓存写入 |
| `src-tauri/src/proxy/usage/logger.rs` | 875 | 请求日志写入、语义记录、冲突与去重 |
| `src-tauri/src/services/sql_helpers.rs` | 213 | fresh input / real total / cache hit rate 的 SQL 口径 |
| `src-tauri/src/services/session_usage.rs` | 2877 | Claude Code JSONL 增量解析、去重、计时估算、同步调度 |
| `src-tauri/src/services/session_usage_codex.rs` | 3071 | Codex JSONL 解析、累计/增量 Token、续写页与子代理处理 |
| `src-tauri/src/services/session_usage_gemini.rs` | 860 | Gemini CLI JSON/JSONL 会话解析 |
| `src-tauri/src/services/session_usage_opencode.rs` | 1384 | OpenCode SQLite V1/V2 消息解析 |
| `src-tauri/src/services/session_usage_pi.rs` | 2927 | Pi / Oh My Pi JSONL 解析 |
| `src-tauri/src/services/session_usage_grokbuild.rs` | 1683 | Grok Build updates.jsonl 解析 |
| `src-tauri/src/services/session_usage_kimi.rs` | 1090 | Kimi Code 会话解析 |
| `src-tauri/src/services/session_usage_dsh.rs` | 1561 | DeepSeek Harness JSONL/Zstd 解析 |
| `src-tauri/src/services/session_usage_agy.rs` | 1184 | Antigravity SQLite/转录解析 |
| `src-tauri/src/services/session_usage_copilot.rs` | 476 | GitHub Copilot CLI usage_record 解析 |
| `src-tauri/src/services/session_usage_workbuddy.rs` | 414 | WorkBuddy JSONL 解析 |
| `src-tauri/src/services/session_usage_codebuddy.rs` | 528 | CodeBuddy JSONL 解析 |
| `src-tauri/src/services/session_usage_mcode.rs` | 577 | MCode SQLite 解析 |
| `src-tauri/src/services/session_usage_omp.rs` | 273 | Oh My Pi 适配 |
| `src-tauri/src/services/usage_stats.rs` | 3200 | 汇总、趋势、供应商/模型统计、请求日志、定价匹配 |
| `src-tauri/src/services/model_pricing.rs` | 963 | 模型定价、models.dev 同步、自定义价格 |
| `src-tauri/src/services/exchange_rates.rs` | 183 | USD 汇率抓取 |
| `src-tauri/src/database/dao/usage_rollup.rs` | 584 | 日聚合与明细剪枝 |
| `src-tauri/src/commands/usage.rs` | 407 | Tauri 命令层 |
| `src/components/usage/*` | — | 用量页面、图表、表格、定价 UI |

### 2.3 统一数据模型

核心表 `proxy_request_logs` 的关键字段：

- `request_id`：主键，用于去重
- `provider_id`、`app_type`、`model`、`request_model`、`pricing_model`
- `input_tokens`、`output_tokens`
- `cache_read_tokens`、`cache_creation_tokens`
- `input_token_semantics`：`legacy=0`、`total=1`、`fresh=2`
- 分项成本与 `total_cost_usd`
- `latency_ms`、`first_token_ms`、`duration_ms`
- `status_code`、`error_message`、`session_id`
- `provider_type`、`is_streaming`、`cost_multiplier`
- `created_at`、`data_source`

日聚合表 `usage_daily_rollups` 保留：

- 日期、应用、供应商、模型、请求模型、计价模型
- 请求数、成功数
- 四类 Token
- 总成本、平均延迟

去重相关：

- `session_usage_dedup`：永久去重账本，避免明细剪枝后重导
- `session_log_sync`：每个会话文件的同步游标

### 2.4 Token 口径

不同协议对 `input_tokens` 的定义不同：

- Anthropic / Claude：`input_tokens` 已经是 fresh input，缓存单独上报
- OpenAI Responses / Codex：`input_tokens` 包含 cache read，部分还包含 cache write
- Gemini：`promptTokenCount` 包含 `cachedContentTokenCount`

统一归一化：

```text
fresh_input =
  input_tokens                                  if semantics = fresh
  input_tokens - cache_read - cache_write       if semantics = total
  input_tokens - cache_read                     if semantics = legacy

real_total_tokens = fresh_input + output + cache_write + cache_read
cache_hit_rate = cache_read / (fresh_input + cache_write + cache_read)
```

费用：

```text
cost =
  fresh_input / 1e6 * input_price
  + output / 1e6 * output_price
  + cache_read / 1e6 * cache_read_price
  + cache_write / 1e6 * cache_write_price
```

Claude 1 小时缓存写入按 5 分钟缓存写入价格的 1.6 倍计算。

### 2.5 时间指标

- 路由请求：精确 `latency_ms`、`first_token_ms`
- 会话日志：根据时间戳估算耗时和首字时间
- 输出速度：
  - 精确：`output_tokens / ((latency - first_token) / 1000)`
  - 估算：`output_tokens / (latency / 1000)`
- 对短请求、异常高速请求设置门槛，避免统计失真

### 2.6 定价系统

- 内置常用模型价格
- 自定义模型价格
- models.dev 自动同步
- 模型 ID 归一化：去供应商前缀、日期后缀、推理档位、tiered 后缀等
- 费用是估算值，不是供应商实际账单

## 3. 当前同步能力与跨设备 PR

### 3.1 发布版

发布版的 WebDAV / S3 同步是**整库配置快照**：

```text
db.sql + skills.zip + manifest.json
```

它不同步用量日志，也不适合多设备同时写入。

### 3.2 PR #8267

社区 PR `feat(usage): add cross-device usage sync via WebDAV and S3` 提出了更完整的方案：

- 每设备独立目录：`usage-v1/{profile}/devices/{device_id}/`
- `manifest.json` + 不可变、内容寻址的 `parts/*.json.gz`
- 设备只写自己的目录，无共享索引写冲突
- writer nonce 检测复制安装/同身份多写入者
- 请求身份账本跨日汇总后仍可去重
- lineage 处理复制数据库/备份恢复
- Local / All / Device 三种查询范围
- 精度标注：exact / not de-duplicable / not detectable
- 隐私：不共享 prompt、response、API Key、错误信息、文件路径

该 PR 尚未合并，但协议设计非常值得 Token Mirror 参考。

## 4. 与其他项目比对

| 项目 | 采集方式 | 强项 | 不足 |
|---|---|---|---|
| CC Switch | 本地代理 + CLI 会话日志 | CLI 支持广、缓存语义完整、桌面体验好 | 无 headless API；发布版不同步用量 |
| ccusage | 只读扫描 20+ CLI 本地日志 | 轻量、headless、解析器覆盖广 | 无设备模型、无事件级同步、无服务端 |
| LiteLLM | 中心 OpenAI 兼容网关 | 多设备在线请求天然汇总、租户/Key 体系成熟 | 看不到绕过网关的直连流量；部署较重 |
| Helicone | Proxy / Async / OTel | 请求分析、用户/会话属性、云与自托管 | 组件多；不直接读取 CLI 会话日志 |
| Langfuse | SDK / OTel / Gateway | Trace、评测、Prompt、Observability | 不解决 CLI 本地日志回填；存储组件多 |
| OpenLLMetry | OpenTelemetry instrumentation | 标准遥测、语言接入广 | 只是传感器，无存储/计费/设备同步 |

### 结论

对于“复刻 CC Switch Token 统计 + 多设备同步”，最合适的是：

```text
自建设备采集器 + 本地 SQLite Outbox + 中心 Usage API + Web 仪表盘
```

- 保留 CC Switch 的本地日志采集能力
- 复用其 Token 语义、定价和统计算法
- 使用追加式事件模型，而不是同步 SQLite 文件
- 中心服务负责多设备汇总、幂等、定价版本和设备管理
- WebDAV 作为无服务端时的可选同步后端

## 5. Token Mirror 架构

### 5.1 Workspace

```text
token-mirror/
├── crates/
│   ├── tm-core/        # Token 语义、费用、定价、领域类型
│   ├── tm-storage/     # SQLite、迁移、聚合查询、Outbox
│   ├── tm-collectors/  # Claude/Codex/Gemini/OpenCode 等采集器
│   ├── tm-sync/        # 中心 API / WebDAV 同步
│   ├── tm-server/      # Axum HTTP API + 静态 Dashboard
│   └── tm-cli/         # 单二进制入口
├── docs/
└── web/                # 轻量 Web UI
```

### 5.2 事件模型

每条用量都是不可变事件：

- `event_id`：稳定 UUIDv5，跨重传幂等
- `device_id`：设备本地生成并持久化
- `source`：proxy / session / import / api
- `source_app`、`source_record_id`
- `session_id`、provider、model、request_model、pricing_model
- UTC 时间、Token 四桶、输入语义
- 延迟、首字、计时质量
- 状态码、流式标记、成本倍率
- 定价版本、元数据

唯一键：`(device_id, source, source_record_id)`。

### 5.3 多设备同步

#### 中心服务模式（推荐）

```http
POST /api/v1/devices/heartbeat
POST /api/v1/events/batch
GET  /api/v1/pricing
GET  /api/v1/summary?scope=all|device&device_id=...
```

- 设备离线先写本地 Outbox
- 网络恢复后批量上报
- 服务端按 `event_id` 幂等
- at-least-once + 幂等，不追求 exactly-once

#### WebDAV 模式

```text
usage-v1/{profile}/devices/{device_id}/
  manifest.json
  parts/*.json.gz
```

- 每设备只写自己的目录
- 不可变分片 + 内容哈希
- 通过 event_id 合并
- 不共享 SQLite 文件

### 5.4 设备身份

- 首次启动生成 UUIDv4 `device_id`
- 默认设备名取 hostname，可覆盖
- 记录平台、架构、agent 版本、last_seen
- 不使用 MAC 地址等硬件标识
- 检测复制安装：writer nonce / lineage

### 5.5 定价系统

- 内置价格 + models.dev 导入 + 自定义覆盖
- 价格版本化
- 事件记录 `pricing_version`
- 支持 USD/CNY 和可配置汇率
- 未定价模型标记为 `unpriced`，不静默按 0 处理

### 5.6 查询能力

- 总览：请求数、真实 Token、缓存命中率、费用、成功率
- 趋势：按小时/天/周/月
- 热力图
- 供应商统计
- 模型统计
- 设备统计与设备对比
- 请求明细分页与详情
- 输出速度、首字时间、延迟

## 6. 实施顺序

1. **核心口径**：TokenUsage、缓存语义、费用计算、模型归一化
2. **存储层**：设备、事件、定价、聚合、Outbox
3. **采集器**：Claude Code → Codex → Gemini CLI → OpenCode → 其他 CLI
4. **迁移工具**：导入现有 `~/.cc-switch/cc-switch.db`
5. **HTTP API**：summary / trends / providers / models / requests / devices / pricing
6. **同步**：中心批量上报 + WebDAV
7. **Web Dashboard**：多设备视图、图表、筛选、定价管理
8. **部署**：systemd、Docker、备份、监控

## 7. 风险与注意事项

- **费用是估算**：订阅套餐的 API 等价成本不等于实际扣费
- **会话日志供应商归属**：无法可靠判断时不要猜测
- **代理与会话重复**：同一次请求可能同时出现在 proxy log 和 session log，需要 alias/身份账本
- **复制安装**：数据库或配置目录被复制后，设备身份可能重复
- **时区**：事件存 UTC；日聚合需要明确使用设备时区还是查看者时区
- **隐私**：默认只同步用量元数据，不同步 prompt、response、API Key、文件路径
- **价格版本**：改价格后应能重算或至少保留历史定价版本
- **去重精度**：没有稳定请求 ID 的历史汇总无法精确跨设备去重，必须标注
