# 本机部署记录（2026-10-19）

- 私有源码仓库：renshui111/relaypool-personal，main 分支。
- 服务容器：relaypool；镜像：relaypool:personal。
- 管理台：http://127.0.0.1:8787/console/；API：http://127.0.0.1:8787/v1。
- 仅发布宿主机 127.0.0.1:8787，使用非 root 用户与只读根文件系统。
- 当前数据卷：relaypool-personal-data。
- 原 relaypool 容器已删除；workbuddy-api 容器保留。
- 原数据卷 deploy_relaypool-data 保留作回滚。停止旧服务时另备份了 private/cutover-data 与 private/old-container.json，均未上传 Git。
- 原有 5 个上游账号和 5 份加密凭证保留；管理密钥沿用。用户及商业表、用户专属密钥和隧道秘密在副本迁移时移除。
- Go 全量测试、go vet、前端结构和账号／额度／概览／聊天检查通过。部署容器健康，32 项实际浏览器与接口检查通过。另在验收副本确认过密钥创建及撤销可写入数据库。
- 未发送真实上游聊天；渠道的实际额度、登录有效性和模型权限仍以上游返回为准。

本机查看管理密钥：`docker exec relaypool /app/relaypool -show-api-key`。
私有备份包含运行凭证，请勿提交到仓库。更新时执行 `docker compose up -d --build`，保留数据卷。
