# Trae2api-cn v1.0.7

本版本修复原生工具流在 DeepSeek harness 中出现的无效 JSON，以及多个工具调用之间的身份和参数隔离问题，并统一模型请求与原生工具流的并发配置。

## 主要更新

- 显式请求 `glm-5.2` 时保留精确模型身份，图片请求也不会静默替换为其他模型。
- 图片内容参与 Remote 输入大小校验。超限图片在 IDE 失败后保留真实错误，不再回落到必然超限的 Remote 请求；可容纳的小图仍保留回退路径。
- 原生工具参数在输出前完成累计和 JSON 验证，结束阶段的修复结果会体现在实际下发内容中，避免客户端收到截断参数后仍以工具调用成功结束。
- 工具调用使用唯一的公开索引，隔离多个调用的 ID、名称和参数，避免上游复用索引时交叉拼接。
- Messages 工具流严格校验 JSON 对象，正确处理空参数，并透传并行工具调用策略及 `disable_parallel_tool_use`。
- 新增统一配置 `TRAE_MODEL_REQUEST_PARALLEL_LIMIT`，默认值为 `4`，范围为 `1-64`。模型请求和原生工具流共用配置容量；旧变量 `TRAE_WEB_PARALLEL_LIMIT` 仍在新变量未设置时兼容生效。
- 取消、排队与响应关闭保留完整的槽位生命周期。配置只控制 relay 本地容量，上游仍可按账号实施更低的并发或频率限制。

## 验证

- 源码打包及授权助手专项测试：`50 passed, 6 skipped, 30 subtests passed`。
- 工具 JSON、多个调用隔离、Messages 严格流和并发容量已补充定向回归测试。
- 最终全量测试：`986 passed, 6 skipped, 458 subtests passed`。
- 候选镜像已验证健康，版本 `1.0.7`，revision 为发布提交，`in_flight=0`，统一容量为 `4`。
- DeepSeek `deepseek-v4.1-flash` 严格探针通过：Chat Completions 与 Messages 均返回合法工具 JSON；同一 session 的 4 路并发请求全部成功，观测最大 `in_flight=4`，工具路径未串线。

## 发布物

GitHub Actions 会生成：

```text
trae-cn-relay-1.0.7.tar.gz
trae-cn-relay-1.0.7.zip
trae-cn-relay-1.0.7.sha256
trae-auth-helper-1.0.7.zip
ghcr.io/autumnsentiment/trae2api-cn:1.0.7
ghcr.io/autumnsentiment/trae2api-cn:latest
```

更新镜像部署：

```bash
docker compose -f docker-compose.image.yml pull
docker compose -f docker-compose.image.yml up -d
```
