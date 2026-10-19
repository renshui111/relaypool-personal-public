本项目的完整启动包和容器包含基于 Cloudflare cloudflared 2026.10.0 的安全依赖更新版本，采用 Apache License 2.0；上游许可证与实际依赖许可证随包保留。

上游源码：https://github.com/cloudflare/cloudflared/tree/2026.10.0
固定源码提交：18cdfe0a6fc7b72a0702d255a1f984e776ce0498
本项目构建版本：2026.10.0-relaypool-security.2

变更：Go 工具链更新至 1.26.9；golang.org/x/net 更新至 v0.60.0；golang.org/x/crypto 更新至 v0.57.0；github.com/klauspost/compress 更新至 v1.18.7；必要的间接依赖由 Go 模块解析器同步更新。运行功能源码未修改。完整依赖锁定文件在公开源码的 deploy/cloudflared.go.mod 和 deploy/cloudflared.go.sum；Dockerfile 固定上述源码提交及锁定文件。

本项目仅调用连接器，令牌由用户提供。发行包不包含账号、令牌或 Cloudflare 账户管理权限。
