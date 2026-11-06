# 华为渠道协议参考

本目录提供 relaypool 的华为云码道 CodeArts 与果办 OfficeAce 适配，包含凭证校验、SDK-HMAC-SHA256、V11-HMAC-SHA256、DPoP/PKCE、模型目录、积分查询、每日奖励领取和 OpenAI 流转换。没有集成第三方账号注册、邀请码自动兑换或凭证扫描程序。

协议研究及独立测试向量参考以下源代码；已固定本轮查阅提交，未执行这些项目的脚本或导入其运行数据：

| 项目 | 查阅提交 | 许可与署名 |
| --- | --- | --- |
| [2951461586/CodeArts2API](https://github.com/2951461586/CodeArts2API) | `c11aa7bba77d148ed04dec2c6a67b74cb8732293` | Apache-2.0，Copyright 2026 Lizi |
| [RobbsLuo/Coding2API](https://github.com/RobbsLuo/Coding2API) | `591fed6e152686a1012cf5d83fc3ecd5aa4f77dd` | MIT，Copyright (c) 2026 Hongsheng Luo (Robbs) |
| [Q-shuang-dot/officeace2api](https://github.com/Q-shuang-dot/officeace2api) | `3838298e45d0b6038dad4310f06b1c37fd3a7141` | MIT，Copyright (c) 2026 Q-shuang-dot |

本项目的变更为 Go Provider 集成、密钥和地区隔离、流式首帧与中途错误处理、凭证脱敏、单次刷新令牌并发保护及现有三种客户端协议接入。相关许可全文位于 `licenses/huawei/`，Docker 镜像保留这些文件。协议参考不构成华为官方兼容认证，也不能替代对当前账号的真实授权和调用验证。
