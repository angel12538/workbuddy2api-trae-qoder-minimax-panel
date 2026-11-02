# Trae2api-cn v1.0.5

本版本发布当前 `main` 分支的完整兼容性修复，源码压缩包与多架构 Docker
镜像由 GitHub Actions 自动构建并发布。

## 2026-10-13 补充修复

本次沿用 `1.0.5` 版本号，替换源码包和镜像。镜像构建时生成授权启动器并验证
Python 模板；控制台点击下载后按当前部署生成 IP、外部端口、HTTPS 与子路径，
无需访问 GitHub 或手动修改地址。

- Windows / macOS / Linux 启动器每次启动从同一部署刷新 Python 助手，
  不再复用未更新的旧文件；下载或替换失败时停止执行。
- 授权前检查本机助手目标，绑定其他部署时提示重新下载，不能由网页任意改写。
- 下载目标必须匹配部署或可信代理；拒绝异常端口和脚本注入参数。
- 用临时文件下载，失败时不执行残留文件；脚本不包含账号凭据。
- 拒绝助手目标为文件夹，避免误执行目录中的旧 `__main__.py`。
- 新增 `/web/login/helper-info`，返回当前配置与本地下载入口。
- 可选 Release 备用助手默认仅指向本机；一般使用部署控制台下载的配置版。

SOLO / IDE 直连当前使用 `solo_work_lite` 的 `llm_utils_chat` 封装，没有已验证的
思考强度 / Max 参数映射。已有完整 1M 参数链路位于 Remote Agent，并要求上游
模型具备 `max_mode` 能力；本次不把直连限制误标成模型本身不支持。

## 主要更新

- 修复 IDE / SOLO 模型网关与 Remote 基础地址混用的问题（#9），自定义网关与
  具体端点 URL 在重启和清除设置后都能正确保留。
- 自动路由开启时锁定上游端点选项，手动模式下仍可固定单个端点。
- 排队位置不再写入模型正文，改为 SSE 注释心跳和消费记录字段。
- 增加截断自动续写，覆盖代码块、括号和输出预算截断场景。
- 修复 OpenAI `image_url` 与 Anthropic base64 图片输入，并在需要时选择多模态模型。
- 新增 Anthropic `POST /v1/messages` 与 `/v1/messages/count_tokens` 兼容入口。
- 增强 ZedeX 风格文本工具调用解析，支持多种 `<tool_call>` 参数格式。
- 保留 `thinking` 与正文的边界，工具调用和流式响应可继续按 API 协议消费。

## 验证

- 本地全量测试：`863 passed, 6 skipped, 386 subtests passed`（含动态助手下载、
  模拟授权回调、JavaScript 地址匹配，以及 Windows 启动器的新下载、旧文件
  刷新、下载中断、替换失败和目录目标测试；POSIX 启动器由 Linux CI 验证）。
- 此前部署实测：Chat 非流式 / 流式、Tool 调用、图片输入、`/v1/messages`
  流式与 `count_tokens` 均正常，无上游 4xx / 5xx。
- 本次授权助手修复未启动 SOLO 客户端抓包，也未重做真实模型 A/B 调用。
- GitHub Actions 会在本标签上重新运行测试，并构建 `linux/amd64`、
  `linux/arm64` 镜像。

## 发布物

```text
源码包：trae-cn-relay-1.0.5.tar.gz / trae-cn-relay-1.0.5.zip
镜像：ghcr.io/autumnsentiment/trae2api-cn:1.0.5
```

更新已有部署：

```bash
docker compose -f docker-compose.image.yml pull
docker compose -f docker-compose.image.yml up -d
```
