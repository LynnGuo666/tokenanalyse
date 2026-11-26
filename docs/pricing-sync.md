# 价格表与实时重算

## 设计原则

- 事件只对原始用量负责：Token、缓存、模型、时间、会话、请求 ID。
- 价格是公共配置，由所有节点共享。
- 所有统计查询按当前激活价格表实时计算。
- 入库时的费用只作为审计快照，不参与最终统计。
- 价格更新不需要重写历史事件。

## 价格来源

1. `models.dev`：自动抓取公共模型价格与 canonical model ID。
2. 自定义 JSON：手动覆盖或补充模型。
3. 节点同步：从上级服务器或其他 WebDAV 设备合并。

## 版本与元数据

每个价格版本包含：

- `versionId`
- `source`
- `createdAt`
- `contentHash`
- `models`
- `aliases`
- `sync.lastUpdatedAt`
- `sync.lastCheckedAt`
- `sync.lastSyncedAt`
- `sync.remoteHash`
- `sync.lastError`

## 合并策略

按 `model_id` 增量合并：

- 本地缺失：直接补充。
- 两边相同：跳过。
- 两边不同：按策略处理。
  - `prefer_remote`
  - `prefer_local`
  - `prefer_newer`
  - `keep_both`

合并后生成新的 `merged-<hash>` 版本并激活，不覆盖历史版本。

## 模型别名

别名独立于价格表，可随价格快照一起同步：

- `exact`
- `prefix`
- `regex`

事件保存：

- `modelRaw`：原始日志模型名
- `canonicalModel`：别名解析后的标准模型
- `pricingModel`：最终计价模型

## 实时重算

查询流程：

```text
UsageEvent -> 当前价格表 -> Token 语义归一 -> 分项费用 -> 汇总
```

因此：

- 修改输入/输出/缓存价格后，Dashboard 立即变化。
- 新增模型价格后，未定价历史请求立即参与估算。
- 修改别名后，新采集事件使用新标准模型；历史事件可通过重算/重建映射刷新。
