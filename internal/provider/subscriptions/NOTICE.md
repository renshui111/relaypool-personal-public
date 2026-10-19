# 协议参考与许可

本适配器由 relaypool 在现有号池接口上实现，没有执行或整体引入第三方网关、注册工具或客户端补丁。

- Qoder 桌面 COSY 编码、签名与协议字段参考 [cpa-multi-plugins](https://github.com/mmqz/cpa-multi-plugins/tree/521ae5178ab023a1500fa795570ce82b9d132a5a/plugins/qoder)，固定提交 `521ae5178ab023a1500fa795570ce82b9d132a5a`。MIT 许可，保留 `LICENSE-cpa-qoder.txt`、`LICENSE-cpa.txt` 中的 Sliverkiss、lovingfish、mmqz 署名。
- Trae 国内 HTTP、工具消息与 SSE 事件参考 [cli2api](https://github.com/caigee-cmd/cli2api/tree/a6502521e1e509a4257153025c642240c39cd54b/internal/providers/trae) 和上述 MIT 仓库，固定提交 `a6502521e1e509a4257153025c642240c39cd54b`。保留 `LICENSE-cli2api.txt`。
- Trae 国际 Web SOLO 会话、目录、请求头及累计文本事件参考同一固定提交的 `plugins/trae/intlupstream/client.go`（MIT，mmqz，基于 diegosouzapw 的 OmniRoute 协议实现）。保留上述 MIT 许可证及本署名，不复用其宿主插件或后台流泵。
- [qodercli2api](https://github.com/Liki4/qodercli2api/tree/b8b595fabbed733c5899019fed4b660ea23d94e0) 为 AGPL-3.0，仅阅读 README 和许可证用于方案比较，未复用其代码或夹具。

Qoder 使用桌面协议而非官方 CLI 加密凭证协议。Trae 国际独立渠道通过远程会话协议转换文本回复，不自动创建项目，不支持客户端函数工具与图片。协议互操作以固定源码为依据，真实订阅账号的权限、字段及版本兼容仍待授权账号验收。Qoder COSY 中的兼容标记不用于推断账号套餐或提高权限；实际权限由上游凭证决定。
