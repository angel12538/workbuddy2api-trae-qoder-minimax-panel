# Trae2api-cn v1.0.6

本版本同步当前 FNOS 已部署版本，修复 Remote 超大上下文请求在上游静默结束后被重复轮询并最终返回 502 的问题。

## 主要更新

- Remote 请求对扁平化 `query` 设置 480000 字符上限。
- 多轮历史继续优先删除最旧完整回合；单条超大消息改为保留开头和结尾并加入截断标记。
- 当固定元数据本身无法容纳在限制内时，直接返回 `413 context_length_exceeded`，不再把同一请求重复提交到其他账号。
- 增加单条百万级消息、流式结束帧、Agent/Work 回退和超限错误的回归测试。
- 保留当前已部署的账号、积分校准、端点路由、工具调用、思考强度和模型列表功能。

## 验证

- 本地全量测试：`946 passed, 6 skipped, 418 subtests passed`
- FNOS 容器状态：`healthy`
- 真实链路验证：普通请求、700K 字符非流请求、700K 字符流请求、非流工具调用、流式工具调用均正常结束。
- 700K、1M 和 7M 级单条输入均在发送前压缩到 480000 字符以内。

## 发布物

GitHub Actions 会生成：

```text
trae-cn-relay-1.0.6.tar.gz
trae-cn-relay-1.0.6.zip
trae-cn-relay-1.0.6.sha256
trae-auth-helper-1.0.6.zip
ghcr.io/autumnsentiment/trae2api-cn:1.0.6
ghcr.io/autumnsentiment/trae2api-cn:latest
```

更新镜像部署：

```bash
docker compose -f docker-compose.image.yml pull
docker compose -f docker-compose.image.yml up -d
```
