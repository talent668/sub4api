# 原生 WS 链路与即时取消

## 实际链路

新使用记录的 `transport_details` 只保存实际观察到的协议枚举：客户端协议、上游协议、模式和有限的回退原因。不保存正文、请求头、URL 或凭据。

- `HTTP→WS`：HTTP 客户端通过网关的 WS 桥接访问上游。
- `WS→WS`：两端均使用 WS，模式为 `ctx_pool` 或 `passthrough`。
- `WS→HTTP`：WS 客户端通过 HTTP/SSE 桥接访问上游。
- `HTTP→HTTP`：实际调用 HTTP 上游。

历史数据或没有可靠观测的记录显示“链路未记录”，不根据旧 `openai_ws_mode` 或请求类型猜测。WS 失败后转到 HTTP 的记录显示最终 HTTP 链路及回退原因。

## 原生 WS 优先，允许 HTTP 回退

创建或编辑 OpenAI OAuth、Setup Token 或 API Key 账号时，勾选“原生 WS 优先（允许 HTTP 回退）”。此预设保存 `extra.openai_native_ws_preferred=true`，固定使用受账号连接池上限约束的 `ctx_pool`，并与“仅原生 WS”互斥。默认不替现有账号开启，不修改全局、其他账号、代理或凭据。

通过管理 API 设置时，必须同时显式选择该账号类型的 `ctx_pool`（或已有等价的 WS 启用字段）；仅提交 preferred 标记会被拒绝，避免继承部署默认 `off`、`passthrough` 或 `http_bridge`。界面会一并写入正确模式。

回退由客户端负责：客户端支持并启用 Responses WS 时先建立原生连接；连接失败后按客户端自身的重试策略发起新的 HTTPS 请求。预设不修改 Kilo/OpenCode 的本地配置，不保证旧版或普通 Chat Completions 客户端会主动使用 WS。

- 原生 WS 入站要求实际 WS 上游，大首包、插件或关闭 WS 的配置不会使它在网关内部静默桥接到 HTTP。条件不满足会结束该连接，留给客户端决定是否发起独立 HTTP 请求。
- 此账号的 HTTP 入站保持 HTTP，包括 Responses 和 Messages，即使系统开启 HTTP→WS 桥接、分组强制 Fast，也不会重新拨号 WS。该传输约束在本次 HTTP 请求的后续选号尝试中保留；不改变 service tier 或计费策略，也不会把“客户端可能发生过回退”猜写成使用记录的 WS 失败原因。
- 本预设只作用于所选账号及已经锁存策略的当前请求。混合分组的新 HTTP 请求若首先选到普通账号，仍按那个账号和全局设置路由；要让整个分组的 HTTP 回退始终走 HTTP，需给可选号分别应用此预设或关闭系统 HTTP→WS 桥接。
- 此预设不增加网关自动 HTTP 回退、自动重放或重试次数。已有输出、工具执行结果不确定、用户取消及 WS 会话续链均不会因本预设被额外重发；客户端自身的重试边界仍须按版本验证。

WS 仍需下述网关全局开关、对应账号类型和模式路由可用。显式关闭 WS 的配置不会被预设覆盖。正常请求显示 `WS→WS · ctx_pool`，独立 HTTP 请求显示 `HTTP→HTTP`。关闭预设只移除该标记，保留模式和其他账号设置；随后恢复全局桥接的正常决策。

## 仅原生 WS 预设

创建或编辑 OpenAI 账号时可以主动勾选“仅原生 WS 预设”。默认选择受账号连接池上限约束的 `ctx_pool`；管理员仍可明确选择独立 `passthrough`。不会修改其他账号、全局配置、代理或授权凭据。

此账号只用于 WS 客户端。普通 HTTP 入站选号会排除该账号，直接内部 HTTP 调用也会拒绝。连接一旦要求原生 WS，后续换号仍须满足原生 WS 条件；插件、大首包或不支持的上游不能使它静默变成 HTTP 桥。

网关必须已启用 WS/v2、相应账号类型和模式路由，且没有强制 HTTP。条件不满足会明确拒绝。管理员可按现有配置方式启用，例如：

```yaml
gateway:
  openai_ws:
    enabled: true
    mode_router_v2_enabled: true
    responses_websockets_v2: true
    oauth_enabled: true
    apikey_enabled: true
    force_http: false
```

保存账号预设只影响新连接；全局配置变更按现有部署方式生效。关闭预设恢复正常模式选择，不清除账号其他设置。

## 取消

`ctx_pool` 和 `http_bridge` 持续读取本客户端连接上的 `response.cancel`。可携带当前 `response_id`，也可省略以取消当前活动轮。错误、过期或其他连接的响应 ID 不会取消当前轮；重复取消幂等。取消帧有独立大小限制，只转发协议类型和响应 ID。

原生上游优先接收取消控制帧；若不能在有限时间内确认终态，该上游连接不再复用。当前保守实现取消后要求重连，不自动换号或重放整轮输入、工具调用。

HTTP 桥取消仅中止当前 HTTP turn。下一轮必须是自包含的新请求；旧 `previous_response_id` 和工具续链会明确要求重连。桥接历史和继承的工具状态会清空。最多缓冲一条下一轮请求，仍在上一轮结束后逐轮进行认证、计费和并发准入。

已明确报告的 token 用量及已完成图片继续按现有规则记账；没有报告的用量不会被伪造为零消费成功。用户取消不计为上游账号健康故障。`passthrough` 的既有双向取消行为保留。

## 客户端显式关联 WS 失败

使用记录可在实际链路旁显示“已关联 WS 失败”，悬停可见网关生成的失败编号。该标记只说明客户端带回的失败回执已通过校验，不推断客户端是否自动重试。原生 WS 失败本身可能没有用量记录；管理员可用失败编号在结构化日志的 `ws_attempt_id` 中定位发送回执的失败连接。

**当前官方 Kilo/OpenCode 不会自动读取并回传此回执，需要客户端适配。** 仅开启 WS、添加会话 ID 或配置一个静态请求 ID，都不能可靠关联两次请求。没有有效回执、历史记录及存储不可用时，仍只显示实际链路，不按时间、模型、会话或正文相似度补猜。

协议 `receipt-v1`：

1. 客户端建立 Responses WS 时显式发送 `X-Sub4api-WS-Fallback-Correlation: receipt-v1`。
2. 当前只覆盖 `ctx_pool` 的首轮、自包含、至多 256 KiB 的请求；全局预热启用、重复 JSON 键、过深嵌套、工具结果或 `previous_response_id` 续链均不签发。网关只有在确认上游连接获取失败、且这条客户端连接的所有尝试从未开始提交执行输入时，才在最终失败关闭前发送一个有界的 `type: error` 事件：

   ```json
   {
     "type": "error",
     "error": {"type": "server_error", "code": "ws_connection_unavailable", "message": "Upstream WebSocket connection could not be established"},
     "sub4api_fallback": {"receipt": "<opaque receipt>", "ws_attempt_id": "<server-generated UUID>", "expires_in_seconds": 120}
   }
   ```

3. 客户端若按自身策略发起对应的独立 HTTP Responses 请求，携带 `X-Sub4api-WS-Fallback-Receipt: <opaque receipt>`。模型、输入及其他请求字段，以及客户端显式会话/线程请求头必须保持一致；仅 WS 的顶层 `type` 与 HTTP 的 `stream` 作为传输字段忽略，顶层键顺序和 JSON 空白可不同。不做跨协议 Messages/Chat Completions 内容转换匹配。
4. 网关校验当前认证用户、API Key、分组和请求摘要后，原子消费回执。并发最多关联一次；错主体、错正文不消耗正确回执。已关联数据写入 `transport_details.client_ws_failure`，只包含随机失败编号和固定枚举 `receipt_v1`，真实链路仍可能是 `HTTP→HTTP` 或依当前账号设置的 `HTTP→WS`。

回执不授予任何访问权限，不作为计费或幂等 ID，不影响选号、费率、重试次数及是否执行请求。上游写入一旦开始，即使写入报错或暂时没有输出，也视为“可能已执行”；后续尝试不能清除这个状态。用户取消、执行状态不明和后续轮次不会获发回执。

Redis 用于多实例一次性校验，每个 API Key 最多保存 32 条、每条有效 120 秒；只保存回执摘要、带密钥的请求绑定和随机失败编号，不保存正文、原始回执或凭据。发布和校验设置 150ms 的调用预算及独立的 100ms 读写超时，不修改全局 Redis 配置；存储故障仅使关联缺失，不阻断 HTTP。私有关联请求头不会发送到上游，回执不进入 close reason、使用记录或普通日志。
