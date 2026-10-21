# v1.1.12

## 更新内容

- 新增分组级 OpenAI 服务档位 `forced_service_tier`：可选「跟随请求 / 强制 Standard / 强制 Fast / 强制 Ultra Fast」，替换原先只支持强制 Fast 的布尔开关。
- 强制 Ultra Fast 仅对 `gpt-6-astra`（含 `gpt-6` 别名与后缀）、`gpt-6.1-sol`、`gpt-5.6-sol` 写入；其他模型保持客户端传入的档位，不报错、不降级。
- 新增分组级「免费 Ultra Fast」计费开关 `free_openai_ultrafast`，与「免费 Fast」互不影响；`gpt-6.1-sol` 的 Ultra Fast 倍率为 6x。
- HTTP 与 WebSocket `response.create` 两条转发路径行为一致：分组强制值先写入请求体，再由全局 Fast/Flex 策略过滤或拦截。
- 管理后台分组表单改为下拉框，中英文文案同步更新；旧字段 `force_openai_fast` 保留兼容（读取时映射到新字段，响应中作为派生字段返回）。

## 升级说明

- 启动时自动执行迁移 246：新增 `groups.forced_service_tier`、`groups.free_openai_ultrafast` 两列，并把 `force_openai_fast=true` 回填为 `priority`；升级前请备份 PostgreSQL。
- 认证快照版本升至 25，部署后旧快照自动失效，新请求按新字段重建；已建立的 WebSocket 会话需要重连。
- 全局 Fast/Flex 策略仍然生效：`filter` 会移除 `service_tier`，`block` 返回 403；请确认没有 `all + force_priority` 规则把 Ultra Fast 改回 priority。
- Ultra Fast 上游成本更高，开启「免费 Ultra Fast」前请确认计费与利润；回滚到旧二进制前请先确认分组档位配置。
