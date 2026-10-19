# relaypool 本机自用版

从原项目独立拆分，保留多平台账号池、模型反代、管理控制台、聊天、密钥权限、提示词预设、签到、日志统计。删除用户平台、计费支付、公网隧道、局域网及公网配置。

## Docker 部署

运行 node scripts/build-web.mjs，然后 docker compose up -d --build。
管理台：http://127.0.0.1:8787/console/
API：http://127.0.0.1:8787/v1
本机查看初始管理密钥：docker exec relaypool /app/relaypool -show-api-key

宿主机仅绑定 127.0.0.1。数据保存在 relaypool-personal-data 卷中。备份时须同时保留 SQLite 数据库与 credential.key。

## 开发

前端修改 web/src，执行 node scripts/build-web.mjs 更新 Go 嵌入文件。后端使用 Go 1.26.9：go test ./...、go vet ./...。

本轮检查：后端全量测试和静态检查通过；前端账号、额度、概览、聊天及结构检查通过。`scripts/personal-smoke.mjs` 使用真实无头浏览器验证保留页面、鉴权和已删除接口；设置 `RELAYPOOL_SMOKE_BASE` 与 `RELAYPOOL_SMOKE_KEY_FILE` 后运行。不会发送真实模型聊天。

历史迁移 001–012 保持原校验和；013 移除用户和商业表及其密钥，保留账号、上游凭证、管理密钥与普通调用密钥。仅在旧数据库的备份副本上迁移，自用版数据库不支持直接切回完整版。
