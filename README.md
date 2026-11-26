# Token Mirror

一个无需桌面环境的 AI CLI Token 用量统计与多设备同步服务。

Token Mirror 从 Claude Code、Codex、Gemini CLI、OpenCode 等工具的本地会话日志中增量采集用量，统一 Token 语义，按模型定价估算费用，并通过中心 API 或 WebDAV 在多台设备之间汇总。

## 特性

- **Headless**：单二进制 + SQLite + Web Dashboard，无需 Desktop/GUI。
- **多数据源**：本地代理日志、CLI 会话日志、CC Switch SQLite 导入。
- **统一 Token 口径**：fresh input、output、cache read、cache write、real total、cache hit rate。
- **费用估算**：内置定价、models.dev 导入、自定义价格、USD/CNY。
- **时间指标**：延迟、首 Token 时间、输出速度、小时/天/周/月趋势。
- **多设备**：稳定 `device_id`、设备名称、平台、最后在线时间、设备维度统计。
- **同步**：中心服务批量上报 + 本地 Outbox；WebDAV 每设备独立目录。
- **幂等**：稳定 `event_id`，重试、重传、重复导入不会重复计数。

## 架构

```text
Claude/Codex/Gemini/OpenCode 本地日志
                │
                ▼
        Device Collector
                │
                ▼
     SQLite events + outbox
          │           │
          │           ├──> Web Dashboard / Query API
          │           │
          │           ▼
          └──> Server API / WebDAV
                          │
                          ▼
                 多设备汇总与查询
```

Workspace：

- `crates/tm-core`：Token 语义、定价、费用、领域类型
- `crates/tm-storage`：SQLite、迁移、聚合、Outbox
- `crates/tm-collectors`：CLI 会话日志采集器
- `crates/tm-sync`：中心 API / WebDAV 同步
- `crates/tm-server`：Axum HTTP API + 静态 Dashboard
- `crates/tm-cli`：单二进制入口

## 快速开始

```bash
cargo build --release

# 采集一次本地 CLI 用量
./target/release/tm collect --db ./token-mirror.db --device-name "$(hostname)"

# 启动 headless 服务
./target/release/tm serve --db ./token-mirror.db --listen 127.0.0.1:8787

# 浏览器打开
# http://127.0.0.1:8787
```

API Token：

```bash
export TM_API_TOKEN='replace-with-a-long-random-token'
./target/release/tm serve --db ./token-mirror.db --listen 0.0.0.0:8787
```

请求时携带：

```text
Authorization: Bearer $TM_API_TOKEN
```

## 同步

### 中心服务

设备端：

```bash
./target/release/tm sync \
  --db ./token-mirror.db \
  --server https://usage.example.com \
  --token "$TM_DEVICE_TOKEN"
```

服务端：

```bash
./target/release/tm serve --db ./server.db --listen 0.0.0.0:8787
```

### WebDAV

```bash
./target/release/tm sync \
  --db ./token-mirror.db \
  --webdav https://dav.example.com/remote.php/dav/files/user \
  --webdav-user user \
  --webdav-password app-password \
  --remote-root token-mirror \
  --profile default
```

远程目录：

```text
token-mirror/usage-v1/{profile}/devices/{device_id}/batches/*.json
```

每台设备只写自己的目录，通过稳定 `event_id` 幂等合并。

## 从 CC Switch 迁移

```bash
./target/release/tm import-cc-switch \
  --source ~/.cc-switch/cc-switch.db \
  --db ./token-mirror.db \
  --device-name "$(hostname)"
```

## 开发

```bash
cargo fmt --all
cargo check --workspace
cargo test --workspace
```

## 重要说明

- 费用是估算值，不是供应商实际账单。
- 会话日志无法可靠识别供应商时，不会猜测归属。
- 默认只同步用量元数据，不同步 prompt、response、API Key 或文件路径。
- 历史日汇总如果没有稳定请求 ID，无法精确跨设备去重。

## 致谢与许可

Token Mirror 的 Token 语义、费用计算和部分采集算法参考并提取自 [CC Switch](https://github.com/farion1231/cc-switch)。CC Switch 使用 MIT License，Copyright (c) 2025 Jason Young。详见 `NOTICE`。

Token Mirror 本身同样使用 MIT License。
