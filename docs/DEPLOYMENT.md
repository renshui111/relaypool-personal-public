# Docker 部署

## 运行环境

需要 Git、可用的 Docker 引擎和 `docker compose` 命令。Windows 可在 Docker Desktop 的 Linux 容器模式运行；Linux 需让当前用户能够执行 Docker。构建期间需要访问容器镜像仓库和 Go 模块代理。

标准部署只需要 Docker，不需要在宿主机安装 Go、Node.js、SQLite 或 Web 服务器。项目自行构建镜像，没有要求从其他账号拉取成品镜像。

## 安装与启动

在 Windows PowerShell 或 Linux Bash 执行：

```console
git clone --branch personal --single-branch https://github.com/renshui111/relaypool-personal-public.git
cd relaypool-personal-public
docker compose up -d --build
docker compose ps
docker exec relaypool /app/relaypool -show-api-key
```

访问 `http://127.0.0.1:8787/console/` 并使用管理密钥登录。新安装的账号池为空；公开仓库不会为你恢复维护者的账号和配置。

管理密钥在首次启动时生成，并在数据卷中加密保存。启动日志不显示密钥。`-show-api-key` 只适合读取仍然有效的初始密钥，不是重置管理密钥的命令；初始密钥被撤销时请使用已有管理密钥。

## 容器和端口

`compose.yml` 使用以下固定资源：

| 资源 | 名称 |
| --- | --- |
| Compose 项目 | `relaypool-personal` |
| 服务／容器 | `relaypool` |
| 本地镜像 | `relaypool:personal` |
| 数据卷 | `relaypool-personal-data` |
| 宿主机端口 | `127.0.0.1:8787` |

容器内部监听 `0.0.0.0:8787`，宿主机发布地址固定为 `127.0.0.1`。这让本机客户端可以访问服务；不要把宿主机映射改为 `0.0.0.0`，或自行加公网反向代理，这会超出自用版的默认访问范围。其他 Docker 容器在同一网络中也可能直接访问服务，API 仍然要求密钥。

如果 8787 被占用，把 `compose.yml` 中宿主机映射改成例如：

```yaml
ports:
  - "127.0.0.1:8788:8787"
```

重新执行 `docker compose up -d` 后使用 `http://127.0.0.1:8788/console/`。容器内部端口不变。浏览器授权涉及回调端口；调整端口后应按页面提供的回调方式实际核验，必要时使用完整回调地址或授权码回填。

如果已有同名容器，先检查其用途和数据，不能直接启动第二个同名容器。原版迁移见[维护文档](OPERATIONS.md#从完整版迁移)。多实例还需要分别修改容器名称、宿主机端口与数据卷名称，避免共享数据库。

## 出站代理

代理用于容器访问上游平台，不会开放局域网或公网入口。Compose 已支持从项目根目录 `.env` 读取：

```dotenv
RELAYPOOL_PROXY_URL=http://host.docker.internal:7890
```

复制参考文件后自行修改：

```console
cp examples/env.example .env
```

Windows PowerShell 使用：

```powershell
Copy-Item examples/env.example .env
```

修改后执行 `docker compose up -d`。Docker Desktop 可通过 `host.docker.internal` 访问宿主机；代理本身必须允许容器连入。容器里的 `127.0.0.1:7890` 指向容器自身，通常不是电脑上的代理。

Linux 若没有该主机名，可在 Compose 服务中配置 `extra_hosts: ["host.docker.internal:host-gateway"]`，并确认宿主机代理监听地址及防火墙允许容器访问。实际代理端口以自己的配置为准。

`.env` 中的值只有被 Compose 显式引用才会传入构建或容器。当前文件引用 `RELAYPOOL_PROXY_URL` 与 `RELAYPOOL_COMMIT`；其他设置按[配置参考](CONFIGURATION.md)添加。

## 构建下载失败

Go 模块代理可通过构建参数切换：

```console
docker compose build --build-arg GOPROXY=https://proxy.golang.org,direct
docker compose up -d --no-build
```

此参数只影响 Go 依赖下载，不控制 Docker 镜像拉取、系统包下载或运行时访问上游。它们分别取决于 Docker 引擎及容器的网络配置。

## 日常操作

```console
docker compose ps
docker compose logs --tail 100 relaypool
docker compose logs -f relaypool
docker compose stop
docker compose start
docker compose restart relaypool
```

首次启动后健康检查可能短暂显示 `starting`。`/healthz` 用于存活检查，`/readyz` 用于数据库就绪检查；没有上游账号也可以返回成功。

容器以 UID/GID `10001:10001` 运行，根文件系统只读，数据卷和临时目录允许写入。恢复数据时需要保持这些目录的写权限，具体操作见[备份恢复](OPERATIONS.md)。
