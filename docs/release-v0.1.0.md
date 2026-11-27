# TokenAnalyse v0.1.0

首个公开版本。

## 功能

- Headless Go 服务端与 CLI
- 15 类 AI CLI 数据源采集
- 完整 Token、缓存、推理 Token、时间、速度、会话与请求日志
- 实时价格计算，支持 models.dev、自定义价格、价格合并与冲突策略
- 模型别名与变体映射
- 多设备混合节点：服务端 + 采集端 + 上级同步客户端
- 中心 API 与 WebDAV 同步，Outbox 幂等与防循环
- 统一任务中心：前台/后台任务、状态、进度、错误
- 并行费用重算：8 worker + 单事务批量更新
- Web Dashboard 与 Wails 桌面 UI

## 产物

### 服务端 / CLI

- `tokenanalyse_0.1.0_linux_amd64.tar.gz`
- `tokenanalyse_0.1.0_linux_arm64.tar.gz`
- `tokenanalyse_0.1.0_darwin_amd64.tar.gz`
- `tokenanalyse_0.1.0_darwin_arm64.tar.gz`
- `tokenanalyse_0.1.0_windows_amd64.zip`
- `tokenanalyse_0.1.0_windows_arm64.zip`

### 桌面 UI

macOS（解压后为 `TokenAnalyse.app`）：

- `tokenanalyse-ui_0.1.0_darwin_amd64.app.zip`
- `tokenanalyse-ui_0.1.0_darwin_arm64.app.zip`
- 同时提供 `.tar.gz` 版本

Windows（可直接运行）：

- `tokenanalyse-ui_0.1.0_windows_amd64.exe`
- `tokenanalyse-ui_0.1.0_windows_arm64.exe`
- 同时提供 `.zip` 版本

Linux（可直接运行）：

- `tokenanalyse-ui_0.1.0_linux_amd64`
- `tokenanalyse-ui_0.1.0_linux_arm64`
- 同时提供 `.tar.gz` 版本

## 运行

```bash
./tokenanalyse node --db ./data/tokenanalyse.db --listen 0.0.0.0:8787
```

浏览器打开：

```text
http://127.0.0.1:8787
```

Docker：

```bash
docker build -t tokenanalyse:0.1.0 .
docker run --rm -p 8787:8787 -v ./data:/data tokenanalyse:0.1.0
```

## 已知限制

- Linux 桌面 UI 需要 GTK3 与 WebKitGTK 4.1。
- macOS 桌面 UI 尚未进行 Apple 签名/公证，首次运行如被 Gatekeeper 拦截，可在终端执行 `xattr -dr com.apple.quarantine /path/to/tokenanalyse-ui` 后重试。
- 费用是公共价格表估算值，不代表供应商实际账单。
- OpenClaw、Hermes 暂无稳定请求级 Token 日志。

## 校验

```bash
go test -count=1 ./...
go vet ./...
go test -race ./internal/task ./internal/sync ./internal/storage ./internal/pricing ./internal/server
```
