# Trae2api-cn v1.0.9

本版本修复长历史和图片请求阻塞服务、模型空回被当作成功，以及原生接口限流后
在同一账号重复请求的问题。

## 主要更新

- 长历史会话匹配改为增量前缀哈希，保留原有会话标识和账号绑定；600 条约
  3 MB 历史的本地耗时从约 4.17 秒降至 14 毫秒。
- Remote 输入压缩避免逐条删除历史时反复编码整张图片；600 条含图历史的
  本地压缩耗时从约 38.9 秒降至 294 毫秒，保留的消息完全相同。依赖前文工具名
  的 renderer 结果继续使用精确检查；无法容纳的图片直接返回明确错误。
- Chat 和 Messages 流式响应只有结束帧、用量或纯空白时返回明确的
  `api_error`，不再发送成功结束信号；非流式 Messages 同样拒绝空结果。
- Remote 同步和流式请求已输出正文、工具、思考或用量后，后续 429/并发异常
  不再触发跨账号重放；重复轮换到同一账号会明确报错。
- Remote 首错误事件保留业务码、用量和账号容量标记。首事件前的账号限流由
  外层轮换账号处理，不再重复请求同一账号的 Work 执行器。
- 未请求返回思考内容时，Remote 的独立思考字段不会被当作正文；隐藏的模型
  输出仍保留不可安全重放的证据，避免重复执行计费请求。
- IDE 非流式采集补齐 `text_delta`、`translated_text` 等事件，与流式解析保持一致。
- 原生业务码 `3004` 作为账号限流处理。限流不会触发同一账号的 raw 到 legacy
  回退；HTTP 429 或结构化 3004 也不会跨网关、跨路径重复发送；429 错误体
  读取中断时仍保留限流状态，正常释放连接和请求槽位。
- 保留合法空工具参数 `{}`、显式请求的思考内容，以及 404/502 的兼容回退。

## 验证

- 本地及 GitHub Actions 全量测试：`1063 passed, 6 skipped, 592 subtests passed`；
  源码编译检查通过。
- 回归覆盖空 EOF、仅结束帧、纯空白、隐藏思考、输出后错误、工具参数和账号轮换。
- 发布工作流运行全量测试，生成源码包、授权助手包，并构建
  `linux/amd64` 与 `linux/arm64` 镜像。
- 发布源码归档校验和一致，包含同步请求和首错误事件回归测试。
- `1.0.9` 与 `latest` 镜像 revision 均为
  `db189e24526714609605780ed26518f23a8a8fab`，共同的 OCI index digest 为
  `sha256:6b81061018fb122c4c3bed5e5d5ef5291af1e61a80c9d9a1b2e6e4f3eb57fa70`。

## 发布物

```text
trae-cn-relay-1.0.9.tar.gz
trae-cn-relay-1.0.9.zip
trae-cn-relay-1.0.9.sha256
trae-auth-helper-1.0.9.zip
ghcr.io/autumnsentiment/trae2api-cn:1.0.9
ghcr.io/autumnsentiment/trae2api-cn:latest
```

镜像发布后，已有部署仍需执行更新才能运行新版本：

```bash
docker compose -f docker-compose.image.yml pull
docker compose -f docker-compose.image.yml up -d
```
