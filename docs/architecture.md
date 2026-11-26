# TokenAnalyse 架构设计

## 1. 目标

TokenAnalyse 是一个跨平台、可 headless 的 AI CLI 用量分析系统，支持：

- 本地采集 Claude Code、Codex、Gemini CLI、Grok Build、OpenCode、Pi、Oh My Pi、MiniMax Code、Kimi Code、GitHub Copilot CLI、CodeBuddy、WorkBuddy、DeepSeek Harness、MCode、Antigravity 等工具的用量。
- 统一 Token 口径：fresh input、output、reasoning、cache read、cache write、cache 5m/1h。
- 实时价格计算：事件只保存原始用量，查询时按当前公共价格表计算。
- 多设备：稳定 device_id、设备名称、平台、最后在线时间。
- 混合节点：同一进程同时是服务端、采集端和上级客户端。
- 同步：中心 API 或 WebDAV，至少一次投递 + 幂等去重。
- 跨平台 UI：Web Dashboard + Wails 桌面应用。

## 2. 进程角色

```text
collect   纯采集端
serve     纯服务端
node      混合节点：服务端 + 采集器 + 上级同步客户端
ui        Wails 桌面 UI，内嵌同一个 Go 后端
```

`node` 是默认推荐模式：

```text
本地 CLI 日志 ──> Collector ──> SQLite / Outbox
                                      │
                                      ├──> 本机 HTTP API / Web UI
                                      │
                                      └──> 上游 Server / WebDAV
```

## 3. 核心包

- `internal/model`：领域模型、Token 语义、费用计算、模型别名。
- `internal/storage`：SQLite、设备、事件、Outbox、游标、价格表、统计查询。
- `internal/collectors`：各 CLI 日志采集器与 CC Switch 导入器。
- `internal/pricing`：models.dev 抓取、自定义价格、价格合并。
- `internal/sync`：中心客户端、WebDAV、同步 Runner。
- `internal/server`：HTTP API、静态 Dashboard。
- `internal/config`：设备身份、默认路径。
- `cmd/tokenanalyse`：CLI。
- `cmd/tokenanalyse-ui`：Wails 桌面应用。

## 4. 事件模型

每条用量是一条不可变事件：

- 身份：`event_id`、`device_id`、`source`、`source_app`、`source_record_id`、`request_id`、`session_id`
- 模型：`model`、`model_raw`、`canonical_model`、`request_model`、`pricing_model`
- Token：input、output、reasoning、cache read、cache write、cache 5m、cache 1h
- 语义：`input_token_semantics`
- 时间：occurred_at、ingested_at、latency、first token、duration、generation duration、speed
- 状态：status code、error、streaming
- 审计：cost multiplier、pricing version、metadata

唯一键：

```text
event_id
UNIQUE(device_id, source, source_record_id)
```

## 5. 增量与去重

### 采集层

- 文件级游标：offset、mtime、state。
- 半行保护：只推进到最后一个完整换行。
- 稳定事件 ID：由 device/source/app/source_record_id 派生。
- 文件重写/截断：重新扫描或安全跳过，依赖事件 ID 幂等。

### 入库层

- `INSERT OR IGNORE`。
- `event_id` 主键 + 来源唯一键。
- 重复事件不进入 Outbox。

### 同步层

- Outbox 至少一次投递。
- 中心按 `event_id` 幂等。
- 增量拉取使用 SQLite rowid cursor。
- WebDAV 每设备独立目录，批次文件不可变。
- 从上游拉取的事件以 `enqueue=false` 入库，避免回传循环。

### 价格层

- 内容哈希比对。
- 按 model_id 增量合并。
- 冲突策略：prefer_remote、prefer_local、prefer_newer、keep_both。
- 合并后生成新版本，不覆盖历史。

## 6. 实时价格

价格表是公共配置：

- models.dev 基础价格
- 自定义覆盖
- 节点间同步
- 版本、内容哈希、最后更新/检查/同步时间、最后错误

事件入库时保存一份费用快照用于审计，但所有统计查询都会按当前激活价格表实时重算。因此价格更新后，无需重写历史事件即可得到新结果。

## 7. 混合节点防循环

- 本机采集和下级上报：`enqueue=true`，进入上游 Outbox。
- 从上游拉取：`enqueue=false`，不再回传。
- WebDAV 上传时跳过本设备目录的下载。
- 所有层级都依赖 `event_id` 幂等，即使出现重复传输也不会重复计数。

## 8. UI

- Web Dashboard：总览、趋势、供应商、模型、请求日志、会话、设备、定价、别名。
- Wails 桌面应用：复用同一 Web 前端，通过反向代理调用本机 Go 后端。
- UI 可连接本机后端或远程服务端。
