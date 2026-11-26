# HTTP API

默认地址：`http://127.0.0.1:8787`

如果配置了 `TM_API_TOKEN`，除 `/api/v1/health` 外都需要：

```text
Authorization: Bearer <token>
```

## 查询

### 健康检查

```http
GET /api/v1/health
```

### 用量汇总

```http
GET /api/v1/summary?from=<unix>&to=<unix>&deviceId=&appType=&providerId=&model=&sessionId=
```

### 趋势

```http
GET /api/v1/trends?bucket=hour|day|week|month
```

### 供应商统计

```http
GET /api/v1/providers
```

### 模型统计

```http
GET /api/v1/models
```

### 请求日志

```http
GET /api/v1/requests?page=1&pageSize=50
GET /api/v1/requests/{event_id}
```

### 会话

```http
GET /api/v1/sessions
GET /api/v1/sessions/{session_id}?page=1&pageSize=50
```

### 设备

```http
GET /api/v1/devices
```

## 上报

### 心跳

```http
POST /api/v1/devices/heartbeat
```

```json
{
  "device": {
    "deviceId": "uuid",
    "name": "Work PC",
    "platform": "linux",
    "arch": "arm64",
    "agentVersion": "0.1.0",
    "sentAt": "2026-11-27T00:00:00Z"
  }
}
```

### 批量事件

```http
POST /api/v1/events/batch
```

```json
{
  "batchId": "uuid",
  "deviceId": "uuid",
  "createdAt": "2026-11-27T00:00:00Z",
  "cursor": 1,
  "events": []
}
```

响应：

```json
{
  "batchId": "uuid",
  "accepted": 10,
  "duplicates": 2,
  "rejected": 0,
  "serverCursor": 1
}
```

### 增量拉取

```http
GET /api/v1/events?cursor=0&limit=1000
```

## 任务中心

所有状态变更操作都通过任务中心执行。后台任务立即返回 `202`，前台任务等待完成后返回 `200`。

```http
GET /api/v1/tasks
POST /api/v1/tasks/collect
POST /api/v1/tasks/reprice
POST /api/v1/tasks/models-dev
POST /api/v1/tasks/import-cc-switch
POST /api/v1/tasks/sync
POST /api/v1/tasks/pricing-import
POST /api/v1/tasks/pricing-merge
POST /api/v1/tasks/aliases/upsert
POST /api/v1/tasks/aliases/delete
```

任务对象：

```json
{
  "id": "reprice-123",
  "type": "reprice",
  "title": "重算费用快照",
  "mode": "background",
  "status": "running",
  "progress": 0,
  "createdAt": "2026-11-27T00:00:00Z"
}
```

## 价格与别名

### 当前价格表

```http
GET /api/v1/pricing
```

### 合并价格表

```http
POST /api/v1/pricing/merge
```

```json
{
  "policy": "prefer_remote",
  "snapshot": {}
}
```

### 同步 models.dev

```http
POST /api/v1/pricing/sync-models-dev
```

### 重算费用快照

```http
POST /api/v1/pricing/recalculate
```

### 别名

```http
GET /api/v1/aliases
POST /api/v1/aliases
DELETE /api/v1/aliases/{alias_id}
```

```json
{
  "aliasPattern": "vendor/model-v1",
  "canonicalModelId": "model",
  "matchType": "exact",
  "scopeApp": "",
  "priority": 100
}
```
