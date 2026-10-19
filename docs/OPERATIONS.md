# 备份、恢复、升级和排错

## 数据在哪里

| 位置 | 内容 |
| --- | --- |
| Docker 卷 `relaypool-personal-data` | SQLite 数据库、独立凭证加密密钥和服务持久设置 |
| 卷内 `relaypool.db` | 上游账号、加密凭证、网关密钥、预设定制和请求日志 |
| 卷内 `credential.key` | 默认独立加密密钥，必须与数据库配套保存 |
| 浏览器本地存储 | 聊天会话、草稿、部分界面偏好 |
| 浏览器会话存储 | 当前管理密钥 |

自定义加密密钥来自环境变量时，应另行安全保存对应环境配置。数据卷备份不会自动包含容器环境变量、宿主机 `.env`、自定义配置文件或浏览器聊天历史。

## 一致性备份

备份时停止服务再复制整个 `/data`，避免只复制一个正在写入的数据库文件而漏掉 WAL。以下命令会短暂中断服务，使用前确认没有正在生成的回复。

Windows PowerShell，在项目根目录执行：

```powershell
$backupRoot = Join-Path (Get-Location) ('backups/' + (Get-Date -Format 'yyyyMMdd-HHmmss'))
New-Item -ItemType Directory -Path $backupRoot -Force | Out-Null
docker compose stop relaypool
if ($LASTEXITCODE -ne 0) { throw '服务停止失败，未开始复制备份' }
try {
    docker cp relaypool:/data $backupRoot
    if ($LASTEXITCODE -ne 0) { throw '备份复制失败，请检查输出' }
} finally {
    docker compose start relaypool
}
```

Linux Bash，在项目根目录执行：

```bash
backup_root="$PWD/backups/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$backup_root"
docker compose stop relaypool || exit 1
trap 'docker compose start relaypool' EXIT
if ! docker cp relaypool:/data "$backup_root/"; then
  exit 1
fi
docker compose start relaypool
trap - EXIT
```

成功后确认备份目录下 `data/relaypool.db` 和 `data/credential.key` 均存在；若使用环境加密密钥，则核对它的独立备份。不要把备份上传到公开仓库。仓库已忽略 `backups/`、`private/`、数据库和密钥文件。

## 恢复到新的数据卷

保留原卷，先把备份恢复到一个新命名卷。以下示例把日期路径换成自己的备份位置；运行前确认不存在同名临时容器和恢复卷。

```console
docker volume create relaypool-personal-restore
docker create --name relaypool-restore-copy -v relaypool-personal-restore:/data alpine:3.22
docker cp ./backups/你的备份日期/data/. relaypool-restore-copy:/data
docker rm relaypool-restore-copy
docker run --rm -v relaypool-personal-restore:/data alpine:3.22 chown -R 10001:10001 /data
```

准备 `private/compose.restore.yml`，内容为：

```yaml
volumes:
  personal-data:
    name: relaypool-personal-restore
```

停止当前服务并应用恢复卷：

```console
docker compose stop relaypool
docker compose -f compose.yml -f private/compose.restore.yml up -d --no-build
docker compose -f compose.yml -f private/compose.restore.yml ps
docker exec relaypool /app/relaypool -show-api-key
```

核对账号、凭证、管理台和模型目录，再验证真实调用。使用覆盖文件恢复后，后续维护也要持续带上同一组 `-f` 参数；直接运行原始 Compose 会切回原卷。不要在验证前删除原数据卷。

## 升级

先备份数据和本地配置，并记录当前 Git 提交与镜像。可以给旧镜像保留一个本地标签：

```console
git rev-parse HEAD
docker tag relaypool:personal relaypool:personal-before-update
git pull --ff-only
docker compose build
docker compose up -d --no-build
docker compose ps
```

有自己的源码改动时，先提交或保留改动，再合并更新，不要直接覆盖。仓库提交了嵌入页面，普通源码更新不要求运行 Node.js；自己修改 `web/src/` 后应先执行 `node scripts/build-web.mjs`。

更新后核对 `/version`、`/readyz`、账号列表、密钥权限及一次短消息。浏览器仍显示旧页面时重新加载；只刷新浏览器不能替换旧镜像里的页面。

## 回滚

代码和镜像回滚不一定能回滚数据库。先读更新记录；若数据库结构不兼容，使用升级前的完整备份恢复到新卷，再配套使用旧镜像。

确认数据库兼容时，可使用此前保留的镜像标签：

```console
docker compose stop relaypool
docker tag relaypool:personal-before-update relaypool:personal
docker compose up -d --no-build --force-recreate
```

数据库需要恢复时，把上面的最后一步换成恢复章节的 Compose 覆盖文件命令。该流程仅适用于你已保留的自用版旧镜像；不要把完整版直接连接到自用版迁移后的数据库。

## 从完整版迁移

自用版沿用历史迁移 001–012 的校验和，并增加迁移 013。数据库第一次由自用版打开时，会删除用户表、商业和支付表、用户专属 API 密钥及隧道／商业相关设置和秘密。

迁移保留上游账号、加密凭证、独立管理／调用密钥和请求日志。用户专属密钥不会变成独立调用密钥。仅在**完整备份的副本**上迁移，不让两个运行实例共用同一个数据库卷。

建议步骤：停止完整版 → 保存旧容器配置及整个数据目录 → 复制到新卷并修正 UID/GID → 停用原入口 → 启动自用版 → 验证账号与权限。旧版数据库、加密材料及原镜像保留到验收完成；回退完整版使用原始备份。

## 常见问题

| 现象 | 排查 |
| --- | --- |
| 8787 或容器名占用 | `docker ps -a`，检查原实例；改宿主机端口或按迁移流程替换 |
| 网页能打开，后台 401 | 使用网关管理密钥；确认没有粘贴上游 API Key、旧撤销密钥或错误后端的密钥 |
| 403 | 调用密钥不能访问管理接口；检查账号／型号白名单，空白名单表示禁止 |
| 模型目录为空 | 确认账号启用、有完整凭证及目录；手填实际型号；检查当前密钥授权 |
| `healthy` 但无法聊天 | 健康检查不验证上游；看账号详情、真实测试及请求日志 |
| 限流、冷却、无可用账号 | 查看上游错误、单号并发和每次尝试；不要靠重复点击测试消耗更多额度 |
| `attempt to write a readonly database` | 恢复卷是否可写，`/data` 是否属于 UID/GID `10001:10001`，不能把数据目录只读挂载 |
| 凭证／秘密解密失败 | 数据库必须配套原 `credential.key` 或原环境密钥；重新生成密钥无法解密旧数据 |
| 浏览器授权不回调 | 使用本机地址，检查端口；按页面回填完整地址或授权码，保留状态字段 |
| 容器无法访问电脑上的代理 | 容器的 localhost 指向自己；检查 `host.docker.internal`、监听地址和防火墙 |
| Token 或剩余额度未知 | 上游未提供完整数据；不是自动计为零，也不是本地计费失败 |
| 清理后聊天历史不见 | 历史在浏览器，不在 Docker 卷；检查浏览器、地址和本地存储是否变化 |
| 删除用户／支付／网络路径返回 404 | 自用版的预期行为，使用新的文档与客户端入口 |

诊断时先运行 `docker compose ps`、`docker compose logs --tail 100 relaypool` 并保留请求编号。提交公开问题前删除日志中的个人身份和私密信息，不附完整数据库、凭证或密钥。
