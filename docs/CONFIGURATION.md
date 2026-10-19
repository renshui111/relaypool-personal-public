# 配置参考

## 配置来源和优先级

启动顺序是默认值 → `-config` 指定的 JSON → 非空环境变量。部分设置还会在 SQLite 中持久化，例如账号、密钥权限和预设定制；它们不是修改 JSON 就会全部重置的临时配置。

`compose.yml` 目前只从 `.env` 引用运行时 `RELAYPOOL_PROXY_URL` 和构建时 `RELAYPOOL_COMMIT`。把其他变量写入 `.env` 并不会自动注入服务：需在服务的 `environment` 中显式声明，或使用 JSON 配置文件。

参考文件：[`examples/config.example.json`](../examples/config.example.json)、[`examples/env.example`](../examples/env.example)。示例不包含真实凭证。

## 环境变量

| 变量 | 作用 |
| --- | --- |
| `RELAYPOOL_LISTEN` | 监听地址；Compose 为 `0.0.0.0:8787`，宿主机映射仍仅本机 |
| `RELAYPOOL_DATA_DIR` | 数据库及默认独立加密密钥目录；容器为 `/data` |
| `RELAYPOOL_RUNTIME_DIR` | 临时运行目录；容器为 `/run/relaypool` |
| `RELAYPOOL_API_KEY` | 启动时登记初始管理密钥；默认自动生成，无需填写 |
| `RELAYPOOL_CREDENTIAL_KEY` | 可选的 32 字节标准 Base64 加密密钥；未设置则从数据目录读取／生成 `credential.key` |
| `RELAYPOOL_PROXY_URL` | 上游请求的出站代理；未指定时程序还检查常见 HTTP(S) 代理环境变量 |
| `RELAYPOOL_ROUTING` | `round-robin`、`weighted-round-robin` 或 `fill-first` |
| `RELAYPOOL_CROSS_PROVIDER_POOL` | 是否允许裸型号跨渠道选号；默认 `false` |
| `RELAYPOOL_ARMOR_DIR` | 提示词预设目录，默认 `prompts` |
| `RELAYPOOL_ARMOR_DEFAULT` | 预设注册表默认名称，不是所有型号的请求级兜底预设 |
| `RELAYPOOL_ARMOR_ENABLED` | 旧开关迁移时的种子；当前实际开关由密钥权限控制 |
| `RELAYPOOL_ACCESS_MODE` | 只接受空值或 `loopback`；`lan`／`public` 会被拒绝 |
| `TZ` | 容器时区，Compose 为 `Asia/Shanghai` |

数据已经加密后不能随意更换 `RELAYPOOL_CREDENTIAL_KEY`，也不能丢弃原 `credential.key`。丢失原加密材料无法通过重新生成密钥恢复旧凭证。不要把真实密钥写入受 Git 跟踪的 Compose 或示例文件。

例如添加路由变量，在 `compose.yml` 的服务 `environment` 下写：

```yaml
RELAYPOOL_ROUTING: weighted-round-robin
RELAYPOOL_CROSS_PROVIDER_POOL: "false"
```

执行 `docker compose up -d` 重新创建服务使环境变量生效。仅 `restart` 不会重新读取 Compose 环境配置。

## 使用 JSON 配置

先把示例复制为本地文件：

```console
cp examples/config.example.json config.local.json
```

PowerShell 使用 `Copy-Item examples/config.example.json config.local.json`。在服务的 `volumes` 中保留原数据卷，并额外加入配置文件；添加启动参数：

```yaml
volumes:
  - personal-data:/data
  - ./config.local.json:/app/config.local.json:ro
command: ["-config", "/app/config.local.json"]
```

`command` 是传给镜像入口 `/app/relaypool` 的参数，不要再次填写程序路径。修改后运行 `docker compose up -d`。已有环境变量仍会覆盖 JSON 中同名设置，例如 Compose 的 `/data`。

## 主要默认值

| 配置项 | 默认值 | 含义 |
| --- | --- | --- |
| `routing` | `round-robin` | 路由策略 |
| `cross_provider_pool` | `false` | 裸型号是否跨渠道 |
| `pool.max_attempts` | `4` | 一次请求最多尝试账号数；允许 1–8 |
| `pool.max_inflight` | `4` | 未明确覆盖时的单账号并发上限；0 表示不限制 |
| `pool.breaker_threshold` | `3` | 同类连续失败触发退避的阈值 |
| `pool.breaker_cooldown` | `30m` | 熔断退避基础时间 |
| `pool.breaker_cooldown_max` | `6h` | 熔断退避上限 |
| `pool.default_cooldown` | `15s` | 没有更具体信号时的冷却时间 |
| `pool.min_rate_limit_cooldown` | `30s` | 限流冷却下限 |
| `pool.max_retry_after` | `10m` | 接受的 Retry-After 上限 |
| `pool.prefer_expiring` | `true` | 同条件下偏好快到期的额度 |
| `pool.expiring_soon` | `168h` | 额度即将到期窗口，不是访问令牌到期时间 |
| `session.enabled` | `true` | 会话账号绑定 |
| `session.ttl` | `30m` | 空闲绑定有效期 |
| `session.capacity` | `10000` | 绑定缓存容量 |
| `upstream.timeout` | `120s` | 非流式上游整次调用超时 |
| `upstream.header_timeout` | `120s` | 流式响应头等待时间 |
| `upstream.idle_timeout` | `300s` | 上游流分块间读取空闲超时 |
| `schedule.catalog_refresh` | `30m` | 模型目录刷新间隔 |
| `schedule.checkin_enabled` | `true` | 平台奖励调度开关，还受单账号设置及平台能力限制 |
| `schedule.checkin_hours` | `[9,21]` | 计划运行小时，按运行环境时区 |
| `schedule.keepalive_enabled` | `true` | 凭证保活调度 |
| `schedule.keepalive_hours` | `[22]` | 保活小时 |
| `schedule.cooldown_gc` | `5m` | 过期冷却清理间隔 |
| `logging.request_log_enabled` | `true` | 保存请求记录 |
| `logging.retention_days` | `7` | 日志保留天数；0 关闭自动清理 |

时间值推荐写带单位的字符串，例如 `"30m"`。调度时间需结合渠道自身的每日去重窗口；修改这些参数不会绕过上游资格或限流规则。
