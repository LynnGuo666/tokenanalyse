# TokenAnalyse

跨平台、可 headless 的 AI CLI Token 用量分析、价格估算与多设备同步服务。

发布与下载：https://github.com/LynnGuo666/tokenanalyse/releases

支持 Claude Code、Codex、Gemini CLI、Grok Build、OpenCode、Pi、Oh My Pi、MiniMax Code、Kimi Code、GitHub Copilot CLI、CodeBuddy、WorkBuddy、DeepSeek Harness、MCode、Antigravity 等数据源。

## 主要能力

- **完整用量事件**：输入、输出、推理 Token、缓存读/写、5m/1h 缓存、请求 ID、消息 ID、会话 ID。
- **时间指标**：总延迟、首 Token、生成时长、输出速度、趋势、热力图。
- **实时价格**：查询时按当前公共价格表计算；价格更新后历史统计立即重算。
- **公共价格表**：models.dev 同步、自定义 JSON、节点间合并、冲突策略、版本/哈希/同步时间。
- **模型别名**：精确、前缀、正则匹配；原始模型、标准模型、计价模型分层保存。
- **多设备**：稳定 device_id、设备名称、平台、最后在线时间、设备维度统计。
- **混合节点**：同一二进制可同时作为服务端、采集端和上级客户端。
- **同步**：中心 API 或 WebDAV；Outbox 至少一次投递；event_id 幂等去重。
- **跨平台 UI**：Web Dashboard + Wails 桌面应用（Windows / macOS / Linux）。

## 快速开始

```bash
go build -o tokenanalyse ./cmd/tokenanalyse

# 单次采集
./tokenanalyse collect --db ./tokenanalyse.db --device-name "$(hostname)"

# 启动 headless 服务
./tokenanalyse serve --db ./tokenanalyse.db --listen 127.0.0.1:8787
```

浏览器打开：

```text
http://127.0.0.1:8787
```

## 混合节点

```bash
./tokenanalyse node \
  --db ./tokenanalyse.db \
  --listen 0.0.0.0:8787 \
  --upstream https://usage.example.com \
  --upstream-token "$TM_DEVICE_TOKEN"
```

一个 `node` 同时提供：

1. 本机 CLI 日志采集
2. 本机 HTTP API / Dashboard
3. 下级设备上报接入
4. 向上级服务器同步

从上游拉取的事件不会再次进入上游 Outbox，避免循环同步。

## 价格表

```bash
# 从 models.dev 同步
./tokenanalyse pricing sync-models-dev --db ./tokenanalyse.db

# 导入自定义价格 JSON
./tokenanalyse pricing import --file ./pricing.json --db ./tokenanalyse.db

# 查看当前价格版本与同步状态
./tokenanalyse pricing show --db ./tokenanalyse.db
```

自定义 JSON 示例：

```json
{
  "models": [
    {
      "modelId": "my-model",
      "displayName": "My Model",
      "inputCostPerMillion": "1.00",
      "outputCostPerMillion": "2.00",
      "cacheReadCostPerMillion": "0.10",
      "cacheCreationCostPerMillion": "1.25",
      "currency": "USD"
    }
  ]
}
```

## 模型别名

```bash
./tokenanalyse pricing aliases \
  --alias "vendor/model-v1" \
  --canonical "model" \
  --match-type exact
```

支持：

- `exact`：精确匹配
- `prefix`：前缀匹配
- `regex`：正则匹配

## 任务中心

所有状态变更操作都会进入统一任务中心：

- 本机日志采集
- 设备同步
- models.dev 价格同步
- 自定义价格导入/合并
- 费用快照重算
- 模型别名新增/删除
- CC Switch 数据导入

任务区分：

- `foreground`：较短操作，API 等待完成
- `background`：长任务，立即返回并轮询状态

Web Dashboard 顶部可打开任务中心查看状态、进度和错误。

## 数据同步

### 中心服务

```bash
# 服务端
./tokenanalyse serve --db ./server.db --listen 0.0.0.0:8787

# 客户端
./tokenanalyse sync --db ./client.db --server http://server:8787 --token "$TM_DEVICE_TOKEN"
```

### WebDAV

```bash
./tokenanalyse sync \
  --db ./tokenanalyse.db \
  --webdav https://dav.example.com/remote.php/dav/files/user \
  --webdav-user user \
  --webdav-password app-password
```

## 从 CC Switch 迁移

```bash
./tokenanalyse import-cc-switch \
  --source ~/.cc-switch/cc-switch.db \
  --db ./tokenanalyse.db
```

## 开发

```bash
go test ./...
go build ./cmd/tokenanalyse
```

桌面 UI 使用 Wails：

```bash
wails build -clean
```

## 目录结构

```text
cmd/
  tokenanalyse/      CLI / headless 服务
  tokenanalyse-ui/   Wails 桌面应用
internal/
  model/             领域模型、Token 语义、费用计算
  storage/           SQLite、设备、事件、Outbox、价格、统计
  collectors/        CLI 日志采集器、CC Switch 导入
  pricing/           models.dev、自定义价格、合并
  sync/              中心 API / WebDAV 同步
  server/            HTTP API + Web Dashboard
  config/            设备身份与路径
docs/                源码分析与架构设计
```

## 重要说明

- 费用是估算值，不是供应商实际账单。
- 会话日志无法可靠识别供应商时，不会猜测归属。
- 默认只同步用量元数据，不同步 prompt、response、API Key 或文件路径。
- 价格统计实时计算；入库费用仅作为审计快照。

## 许可

MIT。Token 语义、费用计算和部分采集算法参考 CC Switch，详见 `NOTICE`。
