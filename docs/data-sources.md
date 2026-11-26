# 数据源与采集范围

TokenAnalyse 内置以下采集器。所有采集器都输出统一的 `UsageEvent`，包含 Token、缓存、时间、速度、会话、请求 ID、模型与供应商信息。

| 采集器 | source_app | 默认路径 / 环境变量 | 说明 |
|---|---|---|---|
| Claude Code | `claude` | `~/.claude` / `CLAUDE_CONFIG_DIR` | `projects/**/*.jsonl`，assistant message usage |
| Codex | `codex` | `~/.codex` / `CODEX_HOME` | `sessions/**/*.jsonl`，token_count / last_token_usage |
| Gemini CLI | `gemini` | `~/.gemini` / `GEMINI_HOME` | `tmp/*/chats/session-*.json/jsonl` |
| Grok Build | `grokbuild` | `~/.grok` / `GROK_HOME` | `sessions` 与 `archived_sessions` 下 `updates.jsonl` |
| OpenCode | `opencode` | `~/.local/share/opencode` / `OPENCODE_DATA_DIR` | `opencode.db`，兼容 V1/V2/混合布局 |
| Pi | `pi` | `~/.pi` / `PI_AGENT_DIR` | Pi session JSONL |
| Oh My Pi | `omp` | `~/.omp` / `OMP_HOME` | OMP session JSONL，与 Pi 存储隔离 |
| MiniMax Code | `minimax` | `~/.minimax` / `MINIMAX_HOME` | 本地会话/请求日志 |
| Kimi Code | `kimi` | `~/.kimi` / `KIMI_HOME` | `wire.jsonl`、subagent、usage.record |
| GitHub Copilot CLI | `copilot` | `~/.copilot` / `COPILOT_HOME` | `session.usage_record` |
| CodeBuddy | `codebuddy` | `~/.codebuddy` / `CODEBUDDY_HOME` | `projects/**/*.jsonl` 与 subagents |
| WorkBuddy | `workbuddy` | `~/.workbuddy` / `WORKBUDDY_HOME` | `projects/**/*.jsonl` 与 subagents |
| DeepSeek Harness | `dsh` | `~/.deepseek` / `DSH_HOME` | `session.v4.jsonl` / 多帧 `.zstd` |
| MCode / MiniMax Code | `mcode` | `~/.mcode`、`~/.minimax` / `MCODE_HOME`、`MINIMAX_DATA_DIR` | 只读 SQLite committed usage projection |
| Antigravity | `agy` | `~/.agy` / `AGY_HOME` | transcript + conversations SQLite 元数据 |

## 其他来源

### CC Switch 导入

```bash
tokenanalyse import-cc-switch --source ~/.cc-switch/cc-switch.db
```

导入 `proxy_request_logs` 明细，不导入缺少请求 ID 的历史日汇总。

### 代理 / API 上报

任何设备都可以通过 API 批量上报统一事件：

```http
POST /api/v1/events/batch
Authorization: Bearer <token>
Content-Type: application/json
```

### 暂不支持

- OpenClaw：当前没有稳定的请求级 Token 日志。
- Hermes Agent：当前没有稳定的请求级 Token 日志。

这些工具如果经过本地代理，仍可通过代理日志或 API 上报接入。
