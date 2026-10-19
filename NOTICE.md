# 自有代码与第三方内容

本项目自有代码按根目录 `LICENSE` 中的 MIT 许可证准备公开发行。第三方代码和资料保留各自许可；根目录 MIT 不重新授权第三方内容，也不代表上游平台授予账号、额度或接口使用权限。

- Qoder、Trae 协议适配的固定来源、署名与许可见 `internal/provider/subscriptions/NOTICE.md` 和同目录的三个 `LICENSE-*.txt`。
- 控制台的 petite-vue 与 Day.js 使用其原始 MIT 许可证，全文及下载记录在 `licenses/web/`。
- Go 运行时与 Go 模块的许可由发行构建工具从实际使用的工具链和模块缓存收集到发行包的 `licenses/go/`；模块版本与文件指纹同时保存。没有这些文件的二进制包不能作为完整发行包。
- 本机研究版本的 `prompts/`、`internal/armor/assets/library.json` 与调研归档不是本项目自有内容，其来源声明不等于每段引用文本的再分发授权。本轮公开核心不包含这些正文。
- DSH 宿主补丁、冷咖啡桌面安装器、闭源功能和独立 Python 评测框架未作为本项目的内置运行时代码发行。

公开版采用新的源码发行目录，避免携带旧 Git 历史、真实账号数据、私有配置与许可未厘清的研究归档。详见 `docs/PUBLIC_RELEASE.md`。
# 浏览器授权与签到协议

新增授权代码为本项目的协议实现。Codex 设备授权参考 OpenAI 的官方登录说明及公开源码；Google 授权使用部署者自己的 OAuth 客户端，不包含第三方客户端密钥。

Qoder 设备授权、积分活动和 Trae 地区授权协议参考 [buddy-proxy](https://github.com/jsongo/buddy-proxy)（MIT，Mayer），活动字段同时核对 [CreditDaddy](https://github.com/techysy/CreditDaddy)（MIT，CreditDaddy contributors）。保留对应声明于 `licenses/protocols/`。Trae 国内 PKCE 与积分确认另参考订阅适配器中已固定的 cli2api 来源。仅用于协议互操作，未执行或集成第三方注册、风控身份生成和客户端目录扫描脚本。快照以 `docs/login-checkin-source-lock-2026-10-10.json` 中的 SHA256 固定。
