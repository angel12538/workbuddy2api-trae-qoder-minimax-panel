# Trae2api-cn v1.0.8

本版本修复多条并发请求链路在流式首事件、账号轮询、原生 IDE/Work
槽位和工具 JSON 边界上的失败，重点覆盖 DeepSeek Messages harness
与 Remote/IDE 并发请求。

## 主要更新

- Remote 流式请求在首事件超时、空响应、429 或模型绑定不匹配时，轮换全部
  可用账号；内部请求不再递归轮询，避免重复占用槽位。
- Remote 流式会话在客户端提前断开时主动关闭未消费的上游并释放槽位；
  确定性的超大输入仍直接返回 `413`，不会跨账号重复发送。
- 原生 IDE/Work 请求与 Web/Remote 共用
  `TRAE_MODEL_REQUEST_PARALLEL_LIMIT`，槽位持有到响应关闭；取消、失败和
  重复关闭路径均幂等回收。
- 账号轮询不再人为限制为两个账号；流式和非流式 native 请求都会在模型输出
  前轮询全部可用凭据。
- Remote 请求体、common params 和 SSE 输出递归归一化 `set`、`tuple`、
  `bytes` 等非 JSON 值，避免并发流在序列化阶段返回 `502`。
- DeepSeek `/v1/messages` 工具流支持累计参数快照去重，并在安全可修复时补齐
  截断 JSON；不可修复的参数会返回明确的 `api_error`。

## 验证

- 全量测试：`995 passed, 6 skipped, 458 subtests passed`。
- `python -m compileall -q src tests` 通过。
- 发布工作流会生成源码压缩包、授权助手包，并构建
  `linux/amd64` 与 `linux/arm64` 的 GHCR 镜像。

## 发布物

```text
trae-cn-relay-1.0.8.tar.gz
trae-cn-relay-1.0.8.zip
trae-cn-relay-1.0.8.sha256
trae-auth-helper-1.0.8.zip
ghcr.io/autumnsentiment/trae2api-cn:1.0.8
ghcr.io/autumnsentiment/trae2api-cn:latest
```

更新镜像部署：

```bash
docker compose -f docker-compose.image.yml pull
docker compose -f docker-compose.image.yml up -d
```
