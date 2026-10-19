# 自用版前端

入口为 `/console/`，由 Go 服务内嵌提供。使用原生 JavaScript、petite-vue 和本地 Day.js，无 npm 依赖安装步骤。

只修改 `web/src/`。执行 `node scripts/build-web.mjs` 同步 `web/dist/` 与 `internal/api/console/`，再重新构建 Docker 镜像。

保留页面：概览、账号池与详情、聊天、提示词预设、请求日志、API 密钥、连接设置。已删除用户平台、用户管理、计费支付及网络配置。

聊天记录按后端地址保存在浏览器本地；管理密钥仅保存在当前浏览器会话。保留旧数据格式，未知用量不显示为零，不执行模型输出中的 HTML。

检查：

```text
node scripts/web-lint.mjs
node scripts/accounts-check.mjs
node scripts/quota-check.mjs
node scripts/overview-check.mjs
node scripts/chat-check.mjs
```

实际容器与浏览器验收运行 `scripts/personal-smoke.mjs`，使用 `RELAYPOOL_SMOKE_BASE` 指定本机地址、`RELAYPOOL_SMOKE_KEY_FILE` 指定本机管理密钥文件。检查只读页面与接口，不发送真实模型聊天。
