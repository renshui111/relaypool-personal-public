# API 使用

## 地址和鉴权

默认服务地址为 `http://127.0.0.1:8787`。多数 OpenAI 兼容客户端的 Base URL 填 `http://127.0.0.1:8787/v1`；直接请求时使用下表完整路径，避免重复拼接 `/v1/v1`。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/v1/models` | 当前密钥有权访问的模型目录 |
| POST | `/v1/chat/completions` | Chat Completions |
| POST | `/v1/responses` | Responses |
| POST | `/v1/messages` | Anthropic Messages |
| GET | `/v1/key/permissions` | 当前密钥的权限 |
| PUT | `/v1/key/armor` | 当前密钥的预设实际启用开关 |

请求头使用 `Authorization: Bearer <网关密钥>`；也兼容 Anthropic 客户端的 `x-api-key`。JSON 请求设置 `Content-Type: application/json`。不要把密钥放到 URL 查询参数中。

无鉴权的探针包括 `/healthz`、`/readyz` 和 `/version`。`/console/` 是静态登录外壳，能够打开页面不表示能访问受保护的管理 API。

## 查询模型

以下 Bash 样例通过交互输入获取调用密钥，不将真实密钥写入脚本：

```bash
read -r -s -p '网关调用密钥: ' RELAYPOOL_KEY
printf '\n'
export RELAYPOOL_KEY
curl --fail-with-body http://127.0.0.1:8787/v1/models \
  -H "Authorization: Bearer $RELAYPOOL_KEY"
```

返回形如 `{"object":"list","data":[{"id":"实际模型ID","object":"model","owned_by":"渠道"}]}`。目录可能同时出现裸型号、带渠道前缀的型号和区域别名；优先选择返回的具体型号。`workbuddy/实际模型ID` 会固定渠道；裸型号的跨渠道行为由配置决定，默认关闭跨渠道号池。白名单受限时，不保证能使用自动选型／区域别名。

目录为空时检查账号是否启用、是否有模型目录，以及当前密钥的账号和模型权限，不要直接使用本文中的占位符发送请求。

## Chat Completions

将 `实际模型ID` 替换为上一步目录中的具体值：

```bash
curl --fail-with-body http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer $RELAYPOOL_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"实际模型ID","messages":[{"role":"user","content":"你好，请简短回复。"}],"max_tokens":128,"stream":false}'
```

流式示例：

```bash
curl --fail-with-body -N http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer $RELAYPOOL_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"实际模型ID","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

流式回复使用 SSE，正常结束包含协议结束信号。连接提前断开时，保留已收到的部分内容并按失败处理，不把缺少结束信号的流当作完整回复。工具、图片、思考及扩展字段的支持取决于具体上游。

## Windows PowerShell

```powershell
$secureKey = Read-Host '网关调用密钥' -AsSecureString
$apiKey = [System.Net.NetworkCredential]::new('', $secureKey).Password
$headers = @{ Authorization = "Bearer $apiKey" }
$catalog = Invoke-RestMethod 'http://127.0.0.1:8787/v1/models' -Headers $headers
$catalog.data | Select-Object id, owned_by

# 替换成已确认可以使用的准确 ID，不自动选择目录中的第一个型号。
$modelId = '实际模型ID'
$body = @{
    model = $modelId
    messages = @(@{ role = 'user'; content = '你好，请简短回复。' })
    max_tokens = 128
    stream = $false
} | ConvertTo-Json -Depth 8
Invoke-RestMethod 'http://127.0.0.1:8787/v1/chat/completions' `
    -Method Post -Headers $headers -ContentType 'application/json; charset=utf-8' `
    -Body ([System.Text.Encoding]::UTF8.GetBytes($body))
```

PowerShell 中 `curl` 可能是其他命令的别名。需要 curl 的 SSE 行为时明确调用已安装的 `curl.exe`。

## Responses 与 Messages

Responses 最小请求正文：

```json
{"model":"实际模型ID","input":"你好，请简短回复。","stream":false}
```

把正文 POST 到 `/v1/responses`。Anthropic Messages 最小正文：

```json
{"model":"实际模型ID","max_tokens":128,"messages":[{"role":"user","content":"你好"}],"stream":false}
```

把正文 POST 到 `/v1/messages`，可以附 `anthropic-version: 2023-06-01` 并使用 `x-api-key` 鉴权。三种入口共享权限和账号池，协议转换不会为上游补出它没有实现的功能。

## 会话与诊断

可设置 `X-RelayPool-Session: 自己的会话编号` 来保持同一会话的账号绑定；绑定仍受权限、账号可用状态和 TTL 约束。`X-RelayPool-Account` 表示优先账号，不是绕过白名单的强制指定接口。

响应可能包含：

| 响应头 | 含义 |
| --- | --- |
| `X-Request-ID` | 本次网关请求编号，可用于关联日志 |
| `X-RelayPool-Account` | 实际服务账号 |
| `X-RelayPool-Provider` | 实际渠道 |
| `X-RelayPool-Attempts` | 尝试次数 |

预设开关请求为 `PUT /v1/key/armor`，正文 `{"enabled":true}`。当前密钥未获准使用时不能自行扩大许可；管理台负责授权。

## 错误处理

Chat Completions 等接口通常返回 `{"error":{"message":"原因","type":"类型","code":"错误码"}}`；Messages 的错误形状按 Anthropic 信封转换。客户端应结合 HTTP 状态、错误内容及请求编号处理。

| 状态 | 常见原因 |
| --- | --- |
| 400 | 缺型号、正文无效、操作／参数不支持 |
| 401 | 网关密钥缺失、无效或已撤销 |
| 403 | 密钥没有管理权限，或账号／型号不在授权范围 |
| 404 | 路径不存在、资源不存在，或调用了自用版已删除接口 |
| 429 | 账号容量、上游限流或额度相关限制；以返回内容为准 |
| 5xx | 上游失败、暂无可用账号、网关转换或存储异常 |

不要对所有 4xx 无限制重试。流式已经返回 HTTP 200 后仍可能在事件流中报告错误，不能只看最初的状态码。单次推理请求正文上限为 32 MiB；具体管理接口有更小的限制。

## 管理 API 索引

下面接口都要求**管理密钥**，推荐日常通过管理台操作。资源 ID 需从列表获取。

| 接口 | 用途 |
| --- | --- |
| `GET /admin/providers` | 当前注册渠道和能力 |
| `GET/POST /admin/accounts` | 账号列表／新建 |
| `GET/PATCH/DELETE /admin/accounts/{id}` | 账号详情／修改／删除 |
| `PUT /admin/accounts/{id}/credential` | 保存凭证 |
| `POST /admin/accounts/{id}/enabled` | 启停账号 |
| `GET /admin/accounts/{id}/inspect/quota` | 查询额度 |
| `GET /admin/accounts/{id}/inspect/catalog` | 查询目录 |
| `POST /admin/accounts/test` | 批量测试，可能消耗真实额度 |
| `POST /admin/accounts/{id}/chat/completions` | 固定账号测试对话 |
| `POST /admin/accounts/login/start` | 开始浏览器授权 |
| `GET /admin/accounts/login/{id}` | 授权状态 |
| `POST /admin/accounts/login/{id}/complete` | 回填授权结果 |
| `POST /admin/accounts/login/{id}/cancel` | 取消授权 |
| `POST /admin/accounts/{id}/checkin` | 手动领取平台奖励 |
| `GET /admin/checkin/schedule` | 查看计划 |
| `GET/POST /admin/keys` | 列出／新建网关密钥 |
| `PATCH/DELETE /admin/keys/{id}` | 修改／撤销密钥 |
| `PUT /admin/keys/{id}/armor` | 设置指定密钥的实际预设开关 |
| `GET /admin/requests` | 请求日志 |
| `GET /admin/requests/{id}/attempts` | 尝试链路 |
| `GET /admin/stats` | 聚合统计 |
| `GET /admin/armor` | 预设目录 |
| `GET/PUT/DELETE /admin/armor/{name}` | 读取／保存／删除预设 |
| `GET /admin/armor/resolve` | 型号匹配预览 |
| `GET /admin/armor/pack` | 导出预设包 |
| `POST /admin/armor/pack/preview` | 预览导入 |
| `PUT /admin/armor/pack` | 导入预设包 |
| `GET /admin/access` | 本机访问状态，非网络配置入口 |

`/user/*`、`/admin/users`、`/admin/commerce`、`/payments/*` 和 `/admin/network` 已移除，不应继续作为客户端集成入口。
