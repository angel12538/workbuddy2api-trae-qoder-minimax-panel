# Trae2api-cn v1.0.10

本版本依据 Trae CN 3.4.4 的账号 entitlement、Work/Code 请求链路和工具流
报告，统一模型请求的账号容量、身份绑定及实际模型能力描述，并修复并发流中
工具参数和空响应的边界问题。

## 主要更新

- 账号记录解析并持久化 `user_pay_identity`、`user_pay_identity_str` 和
  entitlement 中的 `quota.solo_agent_parallel_limit`。免费或未返回明确额度的
  账号使用两个并发槽作为保守默认；付费账号优先使用服务返回的额度，不按套餐
  名称硬编码容量。
- Raw、Web、Remote、IDE 和 Work 请求共用账号级容量池。新请求在有槽账号之间
  按积分优先调度并保留多账号并发；会话续接固定原账号，槽位预留、消费、取消和
  响应关闭均按唯一 reservation 幂等回收。
- Trae CN 3.4.4 的 Work/Code 合并链路补齐 raw-v2 请求字段、头部和 JWT 身份
  校验；绑定会话使用本次选定账号的付费身份，不再读取可变的全局活动账号。
- 模型能力改为只依据当前账号、tier、请求 function 和服务返回的 descriptor：
  多模态、思维强度和上下文窗口均来自实际 descriptor，不再按官方模型名猜测或
  合成 `1M`、`936K` 等能力。
- 工具流关联键保留 `eventSessionId/sessionId`、`planItemId`、`toolCallId` 和
  `toolCallName`，避免并行 plan item 互串参数。DeepSeek Messages 先缓存并校验
  累计参数快照，结束时只发送合法 JSON，空参数仍输出 `{}`。
- 首个模型事件前的并发、空回和可重试错误可安全释放并轮换账号；已产生正文、
  工具或思考输出后不重复重放。空流、仅结束帧、纯空白和非流式空结果均返回明确
  错误，不再伪装成成功响应。

## 验证

- 账号容量、工具流、Remote/Work/Code 身份、模型 descriptor 和空响应回归测试已
  纳入测试套件；当前 focused 回归通过，源码 `compileall` 检查通过。
- 发布工作流会先运行全量 pytest，再生成源码归档和授权助手包，并构建并推送
  `linux/amd64` 与 `linux/arm64` 的 GHCR 镜像。

## 发布物

```text
trae-cn-relay-1.0.10.tar.gz
trae-cn-relay-1.0.10.zip
trae-cn-relay-1.0.10.sha256
trae-auth-helper-1.0.10.zip
ghcr.io/autumnsentiment/trae2api-cn:1.0.10
ghcr.io/autumnsentiment/trae2api-cn:latest
```

更新镜像部署：

```bash
docker compose -f docker-compose.image.yml pull
docker compose -f docker-compose.image.yml up -d
```
