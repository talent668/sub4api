# v1.1.12

## 更新内容

- 修复 Claude Messages → OpenAI Responses 转换遗漏 `speed: "fast"`：请求体中的 Fast 意图现在会转换为 `service_tier: "priority"`，兼容不再要求旧 Fast beta 头的客户端。
- 未指定 `speed` 时继续兼容 `fast-mode-2026-02-01` beta 头；明确 `speed: "standard"` 时不再被旧头误判为 Fast。分组强制与全局 Fast/Flex 策略仍按原顺序生效。
- 新增 6 个请求转换用例和 28 个真实转发测试组合，覆盖 OAuth/API Key、流式/非流式、旧头冲突、分组覆盖及全局过滤；转换包和 service 包完整测试通过。
- 用量记录独立保存上游回声档位 `upstream_service_tier`，管理员及个人「使用记录」页的费用明细均始终显示该行；上游未声明或历史记录无数据时显示「未声明」。管理员专属账号信息不会返回给普通用户。
- 新增 `openai_service_tier_echo_mismatch` 日志，便于排查请求档位与上游回声不一致。ChatGPT Codex OAuth 的 `default` 回声不代表已确认降为 Standard，回声与计费档位继续分开处理。
- 新增分组级 OpenAI 服务档位 `forced_service_tier`：可选「跟随请求 / 强制 Standard / 强制 Fast / 强制 Ultra Fast」，替换原先只支持强制 Fast 的布尔开关。
- 强制 Ultra Fast 仅对 `gpt-6-astra`（含 `gpt-6` 别名与后缀）、`gpt-6.1-sol`、`gpt-5.6-sol` 写入；其他模型保持客户端传入的档位，不报错、不降级。
- 新增分组级「免费 Ultra Fast」计费开关 `free_openai_ultrafast`，与「免费 Fast」互不影响；`gpt-6.1-sol` 的 Ultra Fast 倍率为 6x。
- HTTP 与 WebSocket `response.create` 两条转发路径的分组策略保持一致：分组强制值先写入请求体，再由全局 Fast/Flex 策略过滤或拦截。
- 管理后台分组表单使用下拉框，中英文文案同步；旧字段 `force_openai_fast` 保留兼容，读取时映射到新字段，响应中作为派生字段返回。

## 升级说明

- 本次使用最新 main 代码覆盖发布 1.1.12，包含此前 1.1.13–1.1.15 的改进和 Fast 转换修复，不回滚功能。统一使用镜像标签 `1.1.12` 或 `latest`；原 1.1.13–1.1.15 发布不再使用。
- 由于版本号复用，旧的本地 `1.1.12` 镜像不会自动更新；请显式重新拉取镜像并重建容器。已运行 1.1.13–1.1.15 的实例也需显式切换到新镜像，不能依赖语义版本自动升级判断。
- 启动时按需自动执行迁移 246（分组服务档位与免费 Ultra Fast）及 247（`usage_logs.upstream_service_tier`）；已执行的迁移不会重复执行，升级前请备份 PostgreSQL。
- 认证快照版本为 25；从较早版本升级时旧快照自动失效，新请求按新字段重建。已建立的 WebSocket 会话建议重连。
- 全局 Fast/Flex 策略仍然生效：`filter` 会移除 `service_tier`，`block` 返回 403；请确认没有 `all + force_priority` 规则把 Ultra Fast 改回 priority。
- 本次修复保证客户端 Fast 意图正确传递，不保证 OAuth 上游一定提速。开启「免费 Fast / 免费 Ultra Fast」前请确认计费与利润。
