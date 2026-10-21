# v1.1.13

## 更新内容

- 用量记录新增「上游回声档位」`upstream_service_tier`：单独记录上游响应里返回的 `service_tier`（如 `default` / `priority` / `ultrafast`），与出站服务档位分开保存，便于确认上游是否真的接受了 Fast / Ultra Fast。
- 后台用量「费用明细」新增「上游回声档位」一行，仅在上游响应声明了档位时显示。
- 新增日志 `openai_service_tier_echo_mismatch`：出站档位与上游回声不一致时输出 `request_id`、账号、模型与两个档位，便于线上排查。

## 升级说明

- 启动时自动执行迁移 247：为 `usage_logs` 增加 `upstream_service_tier` 列；升级前请备份 PostgreSQL。
- 历史数据该列为空，不影响既有用量展示；新请求开始记录。
- 注意：ChatGPT Codex 私有后端对 Fast 请求可能仍回 `default`，上游回声为 `default` 不代表 Fast 未生效；API Key 直连官方账号的回声更具参考价值。
