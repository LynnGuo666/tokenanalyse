# 多设备与混合节点同步

## 角色

```text
collect   只采集本地日志
serve     只提供 HTTP API / Dashboard
node      服务端 + 采集器 + 上级同步客户端
ui        桌面 UI，调用本机后端
```

## 中心服务模式

```text
Device A ──┐
Device B ──┼──> Server / Node ──> Dashboard
Device C ──┘
```

设备端：

```bash
tokenanalyse sync --server https://usage.example.com --token "$TM_DEVICE_TOKEN"
```

服务端：

```bash
tokenanalyse node --db ./server.db --listen 0.0.0.0:8787
```

## 防循环

- 本机采集和下级上报：写入 Outbox，目标为 `upstream`。
- 从上游拉取：`enqueue=false`，不再进入 Outbox。
- 所有事件使用稳定 `event_id`，重复传输不会重复计数。

## WebDAV 模式

```text
tokenanalyse/usage-v1/{profile}/devices/{device_id}/
  batches/*.json
  pricing.json
```

- 每台设备只写自己的目录。
- 批次文件不可变。
- 下载其他设备批次后按 `event_id` 幂等合并。
- 价格文件按内容哈希和模型 ID 合并。

## 增量

- 采集器：文件 offset / mtime / state 游标。
- Outbox：pending 批量投递，失败重试。
- 中心拉取：SQLite rowid cursor。
- 价格：内容哈希 + 按模型增量合并。

## 重复处理

```text
event_id PRIMARY KEY
UNIQUE(device_id, source, source_record_id)
INSERT OR IGNORE
```

同步响应会分别返回：

- `accepted`：新增
- `duplicates`：重复
- `rejected`：拒绝
