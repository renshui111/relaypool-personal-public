# WorkBuddy 桌面端身份试验

2026-11-07：增加可切换的请求头配置，复现用户提供的方案 B 中的身份字段。直接在 Go 适配器构造请求时应用，无需 TLS 解密代理、自签根证书或关闭证书校验。

## 范围

仅国内 WorkBuddy 聊天（包含流式、非流式聚合）和模型目录应用该配置；国际区、登录、令牌续期、积分和签到保持原协议。此项不是启动官方桌面程序，也不模拟桌面端 TLS 指纹，不保证上游如何识别请求。

聊天与目录目标地址仍为 `https://copilot.tencent.com`，没有根据 SNI 连接数量猜测或改写业务目标地址。鉴权账号、请求内容、工具参数、请求关联 ID 和私有数据开关沿用原处理。

| 字段 | 桌面端试验值 |
| --- | --- |
| User-Agent | `WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.161.0` |
| X-IDE-Type / X-IDE-Name | `WorkBuddy` |
| X-IDE-Version / X-Product-Version | `5.9.3` |
| X-Domain | `workbuddy.cn` |
| Origin | `https://www.workbuddy.cn` |
| Referer | `https://www.workbuddy.cn/` |
| X-Agent-Intent / Purpose / Type / Runtime | `craft` / `conversation` / `main` / `local-agent` |

字段组合来自用户提供的测试说明，尚未独立抓取官方客户端确认。[官方更新日志](https://www.workbuddy.cn/docs/workbuddy/Changelog)确认存在 5.9.3 版本，但不能据此验证上述全部请求头。

## 配置与回退

默认保留 CLI 模式。配置文件须通过服务器 `-config` 参数加载：

```json
{
  "upstream": {
    "workbuddy_identity": "desktop-5.9.3"
  }
}
```

环境变量 `RELAYPOOL_WORKBUDDY_IDENTITY=desktop-5.9.3` 可覆盖文件。允许值为空、`cli` 或 `desktop-5.9.3`，其他值拒绝启动。桌面端配置在对应请求上优先于 `upstream.user_agent`，避免出现混合身份。

Compose 默认将该变量设为 `cli`；通过 Compose 选择桌面端时应设置同名环境变量或项目 `.env`，再重建容器。恢复为 `cli` 并重建容器即可回退。保留当前代理设置；`upstream.proxy_url` 或 `RELAYPOOL_PROXY_URL` 控制出站代理，`workbuddy.proxy_url` 不是本版本支持的配置路径。

## 本次验证

- `go test ./...`、`go vet ./...` 全部通过。
- 新增测试在最终 HTTP 请求发送边界检查目录、普通对话、流式对话的身份头、目标主机和鉴权传递；覆盖原 CLI 与国际区保持原行为。
- 积分、签到、续期请求未套用桌面端头；配置文件、环境覆盖、非法值拒绝均有测试。
- 已构建 `relaypool:desktop-trial`，用独立 SQLite 在线备份和独立容器测试，没有替换正式容器。测试副本关闭自动签到和保活，只启用 WorkBuddy 账号。
- 三个国内账号的访问凭证均过期。CLI 与桌面端模式的真实目录请求均返回 HTTP 401，目录数量为 0。
- 对正式账号尝试原有续期接口，三个账号均返回 HTTP 401、业务码 `12153`（登录会话失效）；未写回新的凭证。
- 普通对话、流式、工具调用和多轮的桌面端真实成功验证尚未完成，需要至少一个账号重新授权。没有将他人报告中的 77/38 个模型、29 条请求或通过结果当作本机结果。

正式容器继续运行原版本。重新授权任意一个 WorkBuddy 账号后，应重新制作测试副本，完成真实目录、普通/流式对话、工具调用及结果回传、多轮测试；通过后再启用正式环境。副本中的凭证不能覆盖正式账号。
