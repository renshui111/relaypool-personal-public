# relaypool 控制台前端

控制台由网关同源提供，入口为 `/console/`。使用原生 JavaScript、petite-vue 声明式绑定和 `#/overview` 等哈希路由；没有 npm 依赖安装步骤或前端打包器。页面依赖、字体和素材使用仓库内资源，不从外部 CDN 加载。

## 源码与构建关系

| 路径 | 用途 |
| --- | --- |
| `web/src/index.html` | 页面布局、组件模板和交互绑定 |
| `web/src/app.css` | 设计变量、基础排版、按钮、表单、表格等通用组件 |
| `web/src/theme.css` | 控制台外壳、各页面布局及桌面／手机响应式样式；在 `app.css` 后加载 |
| `web/src/app.js` | 入口、路由、共享状态及部分页面操作 |
| `web/src/api.js` | 请求、鉴权、超时与错误处理 |
| `web/src/overview.js`、`accounts.js`、`quota.js`、`chat.js`、`network.js` | 对应页面及展示逻辑 |
| `web/src/assets/`、`web/vendor/` | 本地素材及已锁定版本的第三方库 |
| `scripts/build-web.mjs` | 使用 Node.js 标准库替换版本宏并同步构建产物 |
| `web/dist/`、`internal/api/console/` | 生成目录，提交到仓库；后者由 Go `embed` 嵌入程序 |

只编辑 `web/src/`，不要单独修改生成目录。样式调整按上述分工放置，避免重复定义同一组件。构建脚本同时更新两份产物；前端更新后仍需重新编译网关并更新对应运行程序或容器，单纯刷新浏览器不会替换旧程序内的页面。

## 数据与交互约束

- 管理密钥仅保存在当前标签页的 `sessionStorage` 或内存中。兼容读取旧 `localStorage` 密钥后会删除旧副本；退出登录也清除密钥和输入残留。后端地址可以保存在 `localStorage`。
- 隧道令牌不写浏览器持久存储、不回显、不进入配置下载。保存失败或重新读取状态保留当前输入，后台确认保存成功后才清空；离开页面时清除未提交输入。
- 聊天历史继续使用 `relaypool.chat.v1.` 加 `encodeURIComponent(RP.baseUrl())` 的 `localStorage` 键，当前会话使用同一键加 `.active`。保持现有数据结构兼容，按后端分开保存，不静默删除旧会话或草稿。
- 密钥的使用许可、实际启用开关和账号／型号白名单分别保留。只改名称提交部分更新，只改开关调用独立接口，避免旧页面快照覆盖其他人的最新权限。
- 未上报的额度、Token 和状态显示未知；聊天正文作为文本显示，不执行模型输出的 HTML。
- petite-vue 的条件分支使用真实元素，避免 `<template v-if>` 或 `<template v-for>` 的直接条件子节点破坏渲染锚点。保留错误提示、空状态、操作等待与手机入口。

## 构建与快速检查

在项目根目录执行。需要 Node.js；完整浏览器检查建议使用 Node.js 22 或更新版本，并确保可用内置 `WebSocket`。编译网关需要 `go.mod` 指定的 Go 1.26.9 工具链；浏览器检查需要本机 Edge、Chrome 或 Chromium。

以下命令适用于 Windows PowerShell 和 Linux：

```text
node scripts/build-web.mjs
node scripts/web-lint.mjs
node scripts/accounts-check.mjs
node scripts/quota-check.mjs
node scripts/overview-check.mjs
node scripts/chat-check.mjs
node scripts/network-tunnel-check.mjs
```

这些边界检查使用合成数据或本地替身。修改后端时另运行 `go test ./...` 与 `go vet ./...`。历史 `verify-*-delivery.mjs` 检查对应封存交付的文件摘要，不能替代当前改动的回归检查。

## 隔离浏览器检查

`console-check.mjs` 会创建、修改和删除测试账号、密钥及网络配置，必须使用隔离实例。以下命令清除外部服务与连接器覆盖，指定刚构建的程序；脚本会自行创建临时数据目录、测试密钥和独立浏览器配置。

Windows PowerShell：

```powershell
go build -o bin/relaypool.exe ./cmd/server
$env:RELAYPOOL_CONSOLE_BASE = $null
$env:RELAYPOOL_CONSOLE_KEY = $null
$env:RELAYPOOL_TUNNEL_BINARY = $null
$env:RELAYPOOL_BINARY = (Resolve-Path .\bin\relaypool.exe).Path
node scripts/console-check.mjs
```

Linux：

```sh
go build -o bin/relaypool ./cmd/server
unset RELAYPOOL_CONSOLE_BASE RELAYPOOL_CONSOLE_KEY RELAYPOOL_TUNNEL_BINARY
export RELAYPOOL_BINARY="$(pwd)/bin/relaypool"
node scripts/console-check.mjs
```

未自动找到浏览器时，通过 `RELAYPOOL_BROWSER` 指定本机浏览器可执行文件的绝对路径。默认测试端口为 `8796`；冲突时通过 `RELAYPOOL_CONSOLE_PORT` 指定空闲端口。

完整隧道表单回归是可选分支：另行准备不会访问 Cloudflare 的本地模拟连接器，并将其绝对路径设置为 `RELAYPOOL_TUNNEL_BINARY` 后运行。脚本使用合成令牌检查保存、连接状态、失败输入保留、停用与删除；未设置时跳过该分支。不要用真实连接器或真实隧道令牌替代模拟环境，模拟通过也不证明真实公网可达。

不要把 `RELAYPOOL_CONSOLE_BASE` 指向正式服务，也不要用真实管理密钥运行写入测试。实际部署的备份、容器替换与数据保留要求见 [前端交接](../docs/FRONTEND_HANDOFF.md)；接口与行为见 [API 文档](../docs/API.md)、[聊天说明](../docs/CHAT.md)、[密钥权限](../docs/KEY_PERMISSIONS.md) 和 [网络配置](../docs/NETWORK_CONFIG.md)。
