# Kimi Code OAuth

Kimi 账号支持 API Key 和 Kimi Code OAuth 两种接入方式。OAuth 使用 Kimi 官方的设备授权流程，对应 Coding 套餐。

1. 在管理端创建账号，选择 **Kimi → Kimi Code OAuth**，填写账号名称、代理和分组。
2. 点击下一步，获取设备验证码，打开 Kimi 官网授权链接并确认。
3. 页面自动检查授权结果并保存账号；保存失败时可以重试保存。
4. 已有 OAuth 账号可通过账号菜单重新授权或手动刷新令牌。

账号支持现有 Chat Completions、Responses 和 Anthropic Messages 转发，以及 Coding 套餐额度查询。实际可用模型和额度以 Kimi 账号为准。

## 凭据与运行行为

- OAuth 主机固定为 `https://auth.kimi.com`，推理和额度请求固定使用 `https://api.kimi.com/coding`。OAuth 账号不接受自定义转发地址；API Key 账号保持原有行为。
- 授权会话保存于 Redis，最多有效 15 分钟，绑定发起授权的管理员。设备码保留在服务端；轮询遵守上游间隔和 `slow_down`。
- 设备标识随授权生成，令牌刷新沿用该标识与账号代理。代理配置错误时不回退到直连。
- 后台和请求路径共用刷新锁。刷新前重读账号，保存时比较凭据与代理快照，避免覆盖并发重新授权。Redis 不可用时停止授权和刷新。
- Access Token 与 Refresh Token 使用现有账号凭据存储；不写入日志。令牌在到期前刷新，刷新响应未返回新的 Refresh Token 时保留原值。
- 删除账号使用现有账号删除流程；这不等于撤销 Kimi 服务端的授权，可在 Kimi 官网管理已授权设备。

协议依据：[Kimi CLI 官方设计](https://github.com/MoonshotAI/kimi-cli/blob/9ab1286b8fe4e6bcd116949a27ce5e0ac3389c82/klips/klip-14-kimi-code-oauth-login.md)。
