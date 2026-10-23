# v1.1.13

## 更新内容

- 用量记录新增「上游回声档位」`upstream_service_tier`：单独记录上游响应里返回的 `service_tier`（如 `default` / `priority` / `ultrafast`），与出站服务档位分开保存，便于确认上游是否真的接受了 Fast / Ultra Fast。
- 后台用量「费用明细」新增「上游回声档位」一行，仅在上游响应声明了档位时显示。
- 新增日志 `openai_service_tier_echo_mismatch`：出站档位与上游回声不一致时输出 `request_id`、账号、模型与两个档位，便于线上排查。
- 新增可选的 Fast HTTP→WS 桥接（默认关闭）：可在「后台设置 → Gateway → Fast WS 桥接」开关控制；开启后 HTTP/SSE 入站的 OAuth/Codex 账号 priority/ultrafast 请求会改走上游 Responses WSv2；passthrough 账号不参与桥接（仍走原透传 HTTP），WS 不可用时自动回退 HTTP，首个语义输出超时（`gateway.openai_first_output_timeout_seconds`）对桥接的流式请求同样生效。默认关闭以保持既有 HTTP 转发行为，便于 A/B 验证；仅由前端设置控制，不需要修改 config/env。
- Fast HTTP→WS 桥接覆盖 Anthropic `/v1/messages` 兼容桥：复合分组把 Claude 模型路由到 ChatGPT OAuth/Codex 账号时，分组强制 Fast 或客户端 Fast（`priority` / `ultrafast`）的请求同样改走上游 Responses WSv2，不再被 HTTP 入口静默降级；流式与非流式请求均支持，WS 在写出任何下游字节前失败时回退 HTTP，首个语义输出超时与 HTTP 路径同语义。
- 定时测试新增「检测类型」：账号「定时测试」现在可选「降智检测」，一个计划可配置多个 GPT 模型、计费 API Key 与 cron；到点后按模型串行走正常网关发起真实请求并记录「正常 / 疑似降智 / 不确定 / 失败」结果（含预测模型、概率、原因、HTTP 状态）。检测计划不会自动恢复账号状态，只做观测。
- 后台用量「费用明细」新增「传输方式」（WS/HTTP）与「Fast 判定」：不依赖不可信的 `service_tier` 回声，可直接看到 Fast 请求是否走了 WS 桥接。

## 本次重建：标准模式 WS 与全局触发范围

- 全局网关的「Fast WS 桥接」扩展为「HTTP→WS 桥接」，新增「全部符合条件的请求 / 仅 Fast」范围，默认选择全部；标准模式与未传服务档位的 Responses/Messages 请求也可走 WS，桥接本身不升级为 Fast、不改变计费策略。账号级 WS 关闭、显式 force_http 及原协议资格限制仍然生效。
- 旧设置未保存 scope 时按全部处理；已有总开关不会自动开启。若原先已开启桥接，升级后会扩大到符合条件的标准请求，可在全局网关设置改回「仅 Fast」。Chat Completions、API Key、passthrough 和 compact 不在本次扩展范围内。

## 同版本重建修复

- 修复 Messages Fast WS 桥接的非流式增量内容/工具参数恢复、取消后的用量排水、真实上游模型记录、每轮计费去重标识与跨 API Key 会话状态隔离；转换器超时可及时终止 WS 等待，cyber-policy 硬拒绝不再重发 HTTP。
- 修复定时降智检测的计费 API Key 归属校验、单实例重复调度与并发控制、超时后的结果保存和真实模型时间戳；限制诊断响应内存，补齐终态解析和 HTTP 状态展示。诊断使用原有指纹判定算法，不改变判定阈值。
- 本次替换原 `v1.1.13` 标签、二进制发布包及同版本镜像；仍保持 `1.1.13` 版本号。已安装旧版的实例需重新下载发布包或拉取镜像并重建容器，单看版本号无法区分两次构建。

## 升级说明

- 启动时自动执行迁移 247：为 `usage_logs` 增加 `upstream_service_tier` 列；升级前请备份 PostgreSQL。
- 历史数据该列为空，不影响既有用量展示；新请求开始记录。
- 注意：ChatGPT Codex 私有后端对 Fast 请求可能仍回 `default`，上游回声为 `default` 不代表 Fast 未生效；HTTP 上游的 Fast 在部分账号/环境下也可直接生效，请以自己的速度/TPS A/B 为准。
- Fast 桥接默认关闭；在「后台设置 → Gateway → Fast WS 桥接」开启即可，无需修改 config/env（仍需 `gateway.openai_ws.enabled=true` 且未开启 `force_http`，服务端可访问 `wss://chatgpt.com/backend-api/codex/responses`）。
- Anthropic `/v1/messages`（如 Claude Code）经复合分组访问 OpenAI 账号时，Fast 同样受上述 Fast WS 桥接开关控制；账号级 WS 关闭时，分组强制 Fast 可以覆盖账号级 opt-out，客户端自带的 Fast 仍受账号开关约束。
- 启动时自动执行迁移 248：为 `scheduled_test_plans` 增加 `test_type` / `api_key_id` / `models`，为 `scheduled_test_results` 增加降智判定字段；升级前请备份 PostgreSQL。
- 定时降智检测会按模型串行发起真实请求并正常计费；建议先手动跑一次「降智检测」确认 Key、模型和指纹库可用。
