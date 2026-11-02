# 客户端接入

- [通用参数](#通用参数)
- [OpenAI 兼容客户端](#openai-兼容客户端)
- [Codex CLI（Responses API）](#codex-cliresponses-api)
- [Claude Code（Anthropic Messages）](#claude-codeanthropic-messages)
- [智能体一键配置](#智能体一键配置)

## 通用参数

| 参数 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:8790/v1`；局域网访问时把地址换成 `http://<网关 IP>:8790/v1` |
| API Key | 在看板「设置」中生成。本机运行且没有配置任何 Key 时，可以留空或随便填 |
| 模型 | 填 `/v1/models` 返回的 `id`，即官方模型名，例如 `Qwen3.8-Max`。其他写法见[模型命名](models.md#模型命名) |

需要固定走某个区域时，可以给 Key 绑定出口，也可以在请求里带 `X-Realm: cn|intl` 请求头，或者 `?realm=` 查询参数。

## OpenAI 兼容客户端

Cherry Studio、Chatbox、NextChat、Kelivo 等客户端，按上面的通用参数填写即可。

接口是 `POST /v1/chat/completions`，支持：

- 流式与非流式
- 工具调用
- 图像输入
- 思考档位：`reasoning_effort`，也兼容 `reasoning.effort` 和 `thinking.effort` 两种写法

旧版的 `/v1/completions`（使用 `prompt` 字段）不支持，会返回 404。

## Codex CLI（Responses API）

网关实现了 `POST /v1/responses`。Codex 使用的 custom freeform 工具（如 `apply_patch`）会自动转换，工具调用能正常进行。

上游只接受 `function` 类型的工具。Codex 发送的 `namespace`（MCP / multi-agent）、`web_search` 等 Responses 专有类型会被丢弃，并在日志里打印 WARN（同一类型每 10 分钟最多一条）。因此 MCP 工具在这个网关上不可用，但其余工具不受影响。

最简单的方式是用下面的[智能体一键配置](#智能体一键配置)。手动配置的话，在 `~/.codex/config.toml` 里加一个 provider：

```toml
model_provider = "qoder-proxy"
model = "Qwen3.8-Max"

[model_providers.qoder-proxy]
name = "Qoder Gateway"
base_url = "http://127.0.0.1:8790/v1"
wire_api = "responses"
env_key = "OPENAI_API_KEY"
```

API Key 放在环境变量 `OPENAI_API_KEY` 里，或写入 `~/.codex/auth.json`。

## Claude Code（Anthropic Messages）

网关实现了 `POST /v1/messages` 和 `POST /v1/messages/count_tokens`。Claude Code 会自己在地址后面拼上 `/v1`，所以 Base URL **不要带 `/v1`**：

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8790"
export ANTHROPIC_AUTH_TOKEN="<API Key>"
export ANTHROPIC_MODEL="Qwen3.8-Max"
```

和原生 Anthropic API 相比，有以下差异：

| 差异 | 说明 |
|---|---|
| 没有 thinking block | Anthropic 规范要求 thinking block 带服务端签名，网关无法生成，因此默认不输出。客户端能看到正文和工具调用，看不到思考过程 |
| 历史中的 thinking block 会被丢弃 | 网关无法校验它的签名，所以不会转发给上游 |
| 流式请求出错不会重试 | `message_start` 已经发出后不能重新开始，网关会发送 `event: error` 结束这次请求。非流式请求仍然正常重试 |
| `count_tokens` 是本地估算 | 不会请求上游 |

`messages` 里的 system 消息默认会原位改成 user 消息，让顶层 system 每一轮都保持不变，从而保证前缀缓存的命中率（`QD_ANTHROPIC_MSG_SYSTEM`）。

## 智能体一键配置

看板「智能体配置」页会检测本机装了哪些 AI 客户端，然后一键把它们的配置指向这个网关：

| 客户端 | 写入的文件 | 协议 |
|---|---|---|
| Claude Code | `~/.claude/settings.json` | Anthropic Messages |
| Codex CLI | `~/.codex/config.toml`、`~/.codex/auth.json` | Responses API |
| OpenCode | `~/.config/opencode/opencode.json` | OpenAI 兼容 |
| DSH (DeepSeek Harness) | `~/.dsh/settings.yaml`、`~/.dsh/.credentials.yaml` | OpenAI 兼容 |
| Crush (Charm) | `~/.config/crush/crush.json` | OpenAI 兼容 |

- **只做增量修改**：只插入或更新 `qoder-proxy` 这一段配置，原有的注释、缩进和其他 provider 都保留不动
- **可以还原**：写入前会备份原文件（`accounts/agent-backups/`，每个文件保留最近 10 份）。点「还原」后，文件会逐字节恢复到接入前的样子，网关新建的文件也会被删除
- **全部成功才生效**：如果一个客户端要写多个文件，其中任意一个失败，全部回滚
- **自动填好模型表**：写入时会从网关拉取 `/v1/models`，顺便验证 Key 是否有效
- **Key 只写在客户端的配置里**：网关这边只保存 Key 的 sha256 指纹
- **只能改网关所在机器上的配置**：这个功能只接受本机（loopback）访问，远程打开看板或网关跑在容器里时不可用

**客户端装在另一台机器上**（例如网关跑在 NAS 上）时，在客户端所在的机器上运行配置器 CLI：

```bash
# 获取配置器（公开只读端点，不需要面板密码）
curl -fsSL http://<网关地址>:8790/agents/cli -o qoder_agents.py

python qoder_agents.py list                                    # 列出本机已安装的客户端
python qoder_agents.py apply --client dsh \
    --base-url http://<网关地址>:8790 --key <API Key>          # 接入
python qoder_agents.py restore --client dsh                    # 还原
```

`--client` 可选：`claude-code`、`codex`、`opencode`、`dsh`、`crush`。


## 接入 new-api / one-api 等聚合网关

聚合网关把我们当成一个 OpenAI 兼容渠道，最常见的三类失败都在**地址填写**上：

| 现象（聚合网关侧） | 原因 | 正确写法 |
|---|---|---|
| `do_request_failed` / 连接超时 | 地址不可达（容器内 `127.0.0.1` 指向容器自己） | 用宿主 IP 或 `host.docker.internal`，端口默认 8790 |
| 404 `not found: /v1/v1/chat/completions` | base URL 已带 `/v1`，客户端又拼一次 | 只填 `http://<host>:8790`（不带 `/v1`） |
| 401 `api key required` | 渠道 Key 与本网关的 Key 不一致 | 面板「设置 → API Key」复制，或用 `QD_API_KEY` |

自检顺序（在**聚合网关所在容器**里执行）：

```bash
curl -s http://<host>:8790/ping            # 期望 pong
curl -s http://<host>:8790/v1/models        # 期望 JSON 列表（无 Key 时 401 也算通）
curl -s -X POST http://<host>:8790/v1/chat/completions      -H 'Authorization: Bearer <KEY>' -H 'Content-Type: application/json'      -d '{"model":"DeepSeek-Flash","messages":[{"role":"user","content":"hi"}]}'
```

如果第一条就失败，是本机网络/端口问题（不是网关逻辑）；三条都通但聚合网关仍报错，请把网关访问日志里的对应行发出来。

本网关对**重复 `/v1` 前缀**会返回带提示的 404，并在日志里记一条 WARN，便于自证。

