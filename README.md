# relaypool · 本机自用版

把自己的多个 AI 平台账号接入同一个网关，在本机管理账号池、聊天和调用统一 API。

这是从完整版独立拆出的自用分支。保留模型反代和管理工作台，移除了局域网／公网配置、Cloudflare 隧道、用户注册审批、钱包、套餐、计费和支付。宿主机端口只绑定 `127.0.0.1`。

公开源码分支：[personal](https://github.com/renshui111/relaypool-personal-public/tree/personal)。本仓库不附带账号、运行密钥、额度或预构建的公开容器镜像。

## 能做什么

| 功能 | 说明 |
| --- | --- |
| 统一 API | Chat Completions、Responses、Anthropic Messages 和模型目录；流式／非流式回复 |
| 多平台账号 | WorkBuddy、Qoder、Trae、Claude、Gemini、OpenAI、Codex，以及多种官方 API 和兼容服务 |
| 账号池 | 轮询、加权轮询、优先填满；优先级、并发限制、会话绑定、失败换号和冷却熔断 |
| 管理台 | 账号导入和授权、模型及额度查询、账号测试、请求日志和统计 |
| 本机聊天 | 模型选择、流式回复、停止、重试、多会话及浏览器历史 |
| API 密钥 | 管理／调用权限、账号和模型白名单、提示词预设的许可与启用开关 |
| 后台任务 | 模型目录刷新、凭证续期和部分平台的签到／奖励领取 |

不同上游支持的工具、图片、思考参数、模型目录和用量上报不同。适配器存在不代表所有平台功能都支持，详见[账号接入](docs/USAGE.md)。

## 快速启动

先安装并启动 Docker，确保以下命令可用：

```console
docker version
docker compose version
```

Windows PowerShell、Linux Bash 均可执行：

```console
git clone --branch personal --single-branch https://github.com/renshui111/relaypool-personal-public.git
cd relaypool-personal-public
docker compose up -d --build
docker compose ps
docker exec relaypool /app/relaypool -show-api-key
```

首次构建需要下载基础镜像和 Go 依赖。直接部署不需要本机安装 Go 或 Node.js；前端资源已经提交并嵌入程序。修改前端源码后才需要重新生成这些资源。

打开 [管理台](http://127.0.0.1:8787/console/)，粘贴上一步读取的管理密钥。首次安装没有上游账号，先在“账号池”添加自己的账号，再查询模型并测试聊天。

| 项目 | 默认值 |
| --- | --- |
| 管理台 | `http://127.0.0.1:8787/console/` |
| 客户端 Base URL | `http://127.0.0.1:8787/v1` |
| 健康检查 | `http://127.0.0.1:8787/healthz` |
| 容器／镜像 | `relaypool` ／ `relaypool:personal` |
| 持久数据卷 | `relaypool-personal-data` |

`healthy` 表示服务存活；`/readyz` 检查数据库是否可用。这两个状态都不证明上游账号已经可以聊天。

## 第一次使用

1. 在“账号池”选择平台，使用浏览器授权、凭证导入或 API 密钥添加账号。
2. 启用账号，查询上游模型；平台不提供自动目录时填写实际型号。
3. 在“聊天”页选中该账号和型号，发送一条短消息。账号测试和聊天会访问真实上游，可能消耗额度。
4. 在“API 密钥”新建**调用密钥**，授权需要的账号和模型，给本机客户端使用。
5. 在客户端填写 Base URL、调用密钥及模型目录返回的准确模型 ID。

后台管理密钥和上游平台密钥用途不同，不要把上游凭证填进网关的登录框。详细步骤见[使用说明](docs/USAGE.md)，请求样例见[API 文档](docs/API.md)。

## 文档导航

| 文档 | 内容 |
| --- | --- |
| [Docker 部署](docs/DEPLOYMENT.md) | 环境、安装、端口、出站代理、日常启动停止 |
| [使用说明](docs/USAGE.md) | 平台接入、账号池、密钥、聊天、预设和日志 |
| [API 文档](docs/API.md) | 鉴权、模型查询、三种协议、流式调用、错误与管理接口 |
| [配置参考](docs/CONFIGURATION.md) | 环境变量、JSON 配置、默认值及 Compose 覆盖 |
| [备份与维护](docs/OPERATIONS.md) | 数据位置、备份恢复、升级回滚、旧版迁移、故障排查 |
| [开发指南](docs/DEVELOPMENT.md) | 结构、请求链路、前端构建、测试和发布 |
| [安全说明](SECURITY.md) | 密钥和数据保护、问题反馈 |
| [更新记录](CHANGELOG.md) | 自用版变更与验证范围 |

## 数据和升级

账号、加密凭证、网关密钥和设置保存在 Docker 数据卷。备份时同时保留 `relaypool.db` 与 `credential.key`；浏览器聊天历史需要另外保存。

普通停止使用 `docker compose stop`，恢复使用 `docker compose start`。`docker compose down` 保留命名数据卷，**`docker compose down -v` 会删除它**。更新或迁移前按[维护文档](docs/OPERATIONS.md)备份。

旧版数据库迁移会删除用户和商业表、用户专属密钥以及隧道相关秘密。只在备份副本上迁移；不能把迁移后的数据库直接交给完整版回滚。

## 许可与来源

自有代码采用 [MIT License](LICENSE)。第三方代码、协议参考和提示词资料分别保留自己的来源与许可，见 [NOTICE.md](NOTICE.md) 和 `licenses/`；根目录 MIT 不重新授权第三方内容。提示词预设不保证改变上游模型的能力或限制。
