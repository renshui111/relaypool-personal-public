# 开发与验证

## 结构和请求流程

后端为 Go 1.26.9、标准库 HTTP 服务与纯 Go SQLite 驱动；不需要 CGO。前端为原生 JavaScript、petite-vue 和本地第三方资源，嵌入 Go 二进制提供服务。

| 路径 | 职责 |
| --- | --- |
| `cmd/server/` | 配置加载、数据库初始化、HTTP 服务和关闭流程 |
| `internal/api/` | 路由、鉴权、权限过滤、三种协议信封、管理业务 |
| `internal/executor/` | 上游调用编排、重试、会话绑定和流式资源释放 |
| `internal/pool/` | 选号、并发占位、优先级、健康状态和冷却 |
| `internal/provider/` | 各平台的协议、凭证、目录及奖励能力 |
| `internal/store/` | SQLite、迁移、账号、密钥、加密和请求统计 |
| `internal/scheduler/` | 目录、保活、奖励与清理任务 |
| `internal/armor/` | 提示词预设、模型匹配和来源资料 |
| `web/src/` | 前端编辑入口 |
| `web/dist/`、`internal/api/console/` | 已提交的生成资源，后者用于 Go embed |
| `scripts/` | 前端构建、边界检查和浏览器冒烟检查 |
| `deploy/Dockerfile`、`compose.yml` | 容器构建和本机运行 |

调用链路：HTTP 解析 → 密钥鉴权及账号／型号权限过滤 → 预设处理 → Executor → Pool 选号占位 → Provider → 上游响应 → 协议转换及请求日志。流开始后不静默换号拼接回答；流关闭后释放账号容量。

## 本地开发

先确保本机 Go 工具链满足 `go.mod`。后端检查与构建：

```console
go test ./...
go vet ./...
go build -o bin/relaypool ./cmd/server
```

Windows 可使用 `go build -o bin/relaypool.exe ./cmd/server`。需要前端构建或检查时安装 Node.js；实际浏览器冒烟脚本需要支持内置 `WebSocket` 的 Node.js（例如 22 或更高）及 Edge／Chrome／Chromium。

```console
node scripts/build-web.mjs
node scripts/web-lint.mjs
node scripts/accounts-check.mjs
node scripts/quota-check.mjs
node scripts/overview-check.mjs
node scripts/chat-check.mjs
```

只编辑 `web/src/`，再运行构建脚本同步两份生成目录。不要只修改 embed 副本。构建脚本会清理并重建生成目录，所以不要把手工资产放入生成目录。

## 没有本机 Go 时

可在 Go 容器里检查源码，命令适用于 PowerShell 和 Bash；需要可用的 Docker：

```console
docker run --rm --mount "type=bind,source=${PWD},target=/src" -w /src golang:1.26.9-bookworm go test ./...
docker run --rm --mount "type=bind,source=${PWD},target=/src" -w /src golang:1.26.9-bookworm go vet ./...
```

这会在临时容器中下载依赖；频繁开发可另挂缓存卷。不要把正式数据卷挂给单元测试。需要检查共享运行状态时，可额外运行 `go test -race ./...`；该命令需要支持的工具链和平台，本轮文档更新未重新声称通过竞态测试。

## 浏览器冒烟检查

`scripts/personal-smoke.mjs` 检查保留 API 和页面、鉴权、已删除入口以及浏览器异常，不创建账号或发送上游聊天。默认目标是 `127.0.0.1:8788`，请显式指定自己要检查的本机实例。

PowerShell 示例：

```powershell
$env:RELAYPOOL_SMOKE_BASE = 'http://127.0.0.1:8787'
$env:RELAYPOOL_SMOKE_KEY_FILE = (Resolve-Path private/smoke-key.txt).Path
$env:RELAYPOOL_BROWSER = 'C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe'
node scripts/personal-smoke.mjs
```

提前把管理密钥保存到本机 `private/smoke-key.txt`，不要提交。Linux 示例：

```bash
RELAYPOOL_SMOKE_BASE=http://127.0.0.1:8787 \
RELAYPOOL_SMOKE_KEY_FILE="$PWD/private/smoke-key.txt" \
RELAYPOOL_BROWSER=/usr/bin/chromium \
node scripts/personal-smoke.mjs
```

浏览器采用独立临时配置目录，脚本完成后清理；不会使用日常浏览器的用户资料。日常开发仍推荐隔离实例，避免把未审查的新测试指向正式数据。

## 修改约束

- 数据库已发布的迁移不能修改 SQL 内容和校验和；结构调整追加新迁移。
- 管理密钥继续经过数据库鉴权与撤销，不能增加内存中的绕过通道。
- 白名单为空不得退化为全部放行；目录、固定账号、会话绑定和换号应使用一致权限。
- 管理密钥不写浏览器长期存储，聊天正文按文本渲染；未知用量不伪造成零。
- 前端异步读取要处理离页、切换连接和旧响应写回的问题。
- 自用版不重新引入用户、支付、隧道或局域网／公网配置入口。

## 发布和文档维护

公开分支为 `personal`。公开仓库与私有仓库使用独立远端，不要把私有运行目录推到任何远端。发布前检查 Git 状态、改动范围、文档相对链接与示例，修改页面时同步生成资源并重新构建镜像。

从公开仓库新克隆时，`origin` 指向公开仓库，正常使用 `git push origin personal`。维护者的本机可能另有私有 `origin` 和公开 `public` 远端；先用 `git remote -v` 确认目标，不直接照搬其他机器的远端名称。

区分单元测试、模拟检查、真实浏览器检查和真实上游推理：其中一项通过不能代替另一项。当前历史验证范围见[更新记录](../CHANGELOG.md)。功能变化同时更新对应文档，尤其是接口、权限、迁移和恢复步骤。
