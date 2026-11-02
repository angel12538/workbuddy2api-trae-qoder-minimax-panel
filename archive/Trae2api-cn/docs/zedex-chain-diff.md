# ZedeX/trae-local-api 与 trae-cn-relay 请求链路差异对比

对比对象：

- `tmp_zedex_clone`（ZedeX/trae-local-api，Node.js/Express，2.1.0）
- `trae-cn-relay`（本项目，Python/FastAPI，Trae2api-cn）

---

## 1. ZedeX 上游调用链：端点、function、请求体、tools

### 1.1 端点 URL

Base host 由 `TRAE_API_HOST` 覆盖，否则按版本探测：CN=`https://trae-api-cn.mchost.guru`、SG=`https://coresg-normal.trae.ai`、US=`https://coreva-normal.trae.ai`（`src/auth.js`）。

| 用途 | 方法与路径 | 代码位置 |
|---|---|---|
| 主对话 | `POST {host}/api/agent/v3/llm_utils_chat` | `src/trae-client.js` `llmUtilsChat()` |
| 回退 1 | `POST {host}/api/ide/v1/chat` | `chatCompletion()` |
| 回退 2 | `POST {host}/api/agent/v3/create_agent_task` | `createAgentTask()` |
| 模型配置 | `POST {host}/api/ide/v1/get_detail_param` | `getModelDetailParam()` |
| 会话模式 | `POST {host}/api/v1/commercial/chat_mode` | `getChatModes()` |
| Token 刷新 | `POST {host}/cloudide/api/v3/trae/oauth/ExchangeToken` | `src/auth.js` `exchangeToken()` |

### 1.2 `function` 字段取值与选择逻辑

- `FUNCTION_MAP` 声明了 5 个函数：`inline_chat`、`solo_coder`、`chat_v3`、`builder_v3`、`system_diagnosis`（历史 PROGRESS 记录 `fast_apply`/`summary` 上游报 no function config，未采用）。
- `resolveModelOptions()` 规则：
  - `model=auto` 或空 → `function=inline_chat`，**不传** `model` / `config_name`（服务端默认模型）。
  - 命中 `model-config.json` 的模型 → 取该条目的 `function`（默认 `chat_v3`）+ `config_name`。
  - 未登记模型 → `function=chat_v3`，`model` 原样直传。
- `llm_utils_chat` body 里：`function` 必带；**仅当 `function !== 'inline_chat'` 时**才写 `config_name`（并把 `model` 也设成同一个 config 名）；`inline_chat` 传 `config_name` 会被上游 4001 拒绝（项目自己的踩坑记录）。
- `create_agent_task` 不用 `function` 字段，用 `agent_type: "builder_v3"`；`/api/ide/v1/chat` 固定 `function: "chat_v3"`。

### 1.3 请求体字段

| 端点 | 字段清单 |
|---|---|
| `llm_utils_chat` | `messages[{role, content:[{type:"text",text}]}]`（content 必须是数组，字符串会被 Go 结构体拒收）、`function`、`stream`（恒 `true`，非流式也在本地聚合）、`config_name`、`model`、`max_tokens`（钳制 ≤128000）、best-effort 转发 `temperature` / `top_p` / `presence_penalty` / `frequency_penalty` / `stop` / `seed` / `n` |
| `/api/ide/v1/chat` | `model`、`messages`、`stream`、`function:"chat_v3"`，并把内部 options（含 `workspace` 等）**原样 spread 进 body**（实现较随意） |
| `create_agent_task` | `session_id`、`task_id`、`message_id`、`conversation_id`、`user_id`、`messages`、`model`、`stream`、`mode_type:1`、`agent_type:"builder_v3"`、`enable_chat_memory:false`、`workspace_folder`、`workspace_id`、`workspace_path`、`user_input{id,user_input,placeholder_map}`、`ide_version`、`ide_version_code`、`device_id`、`extra_info`(JSON 字符串) |

### 1.4 tools 处理：**纯提示词注入，不走原生 body 字段**

- 三个上游端点的请求体里**从不出现** `tools` / `tool_choice` / `parallel_tool_calls` 字段。
- Anthropic 入口 `/v1/messages`（`src/server.js`）收到 `tools` 后：
  1. 建 `toolMap` 做 Claude Code 名称别名映射（`Read`→`read_file`、`Bash`→`execute_command`、`Grep`→`search_files`、`WebFetch`→`fetch_url` 等，大小写不敏感）；
  2. 把工具清单拼成 system prompt 注入（`<available_tools>` + 要求输出 `<toolcall>{"name":...,"params":{...}}</toolcall>` JSON，并禁止 XML 属性式/arg_key 式写法）；
  3. 检测到 `tool_use`+`tool_result` 的多轮续写时追加"必须继续调用或给出最终答案"的强化指令。
- 输出侧解析：流式用 `<toolcall>`/`<tool_call>` 标签状态机把工具块从正文剥离并转成 `tool_use` 块；`parseToolcallContent()` 支持 **6 种容错格式**（JSON、`<toolcall name=...>`、`arg_key/arg_value`、属性式 XML、修尾逗号/单引号的 JSON、混排提取 JSON）；同时把上游原生 `tool_calls` 透传为 `tool_use`。
- `src/agent.js` 里有一套本地工具执行器（`read_file`/`write_file`/`list_files`/`search_internet`/`fetch_url`/`execute_command`，XML `<tool_call>` 提示词 + 最多 8 轮循环），但 **`server.js` 未 require 它，属于未接线的死代码**。

> 与本项目对照：trae-cn-relay 在 IDE/Solo 端点是**原生 `tools` 字段**（`solo_work_lite` + `tools`/`tool_choice`/`parallel_tool_calls`），Remote/raw 通道才用系统提示词 + `<opencode_tool_call>` 文本桥接。ZedeX 则 100% 提示词注入 + 文本解析。

---

## 2. ZedeX 的降级链顺序

由内到外共 5 层：

1. **传输重试**：`retryWithBackoff()` 只对 4011/429 生效，指数退避 3 次（2s 基数 + 随机抖动）。
2. **端点降级**（流式与非流式相同顺序）：
   `llm_utils_chat` → `/api/ide/v1/chat` → `/api/agent/v3/create_agent_task`。
3. **排队降级**（仅 Anthropic `/v1/messages` 流式路径）：解析上游 `request_wait_in_queue` 的 `position`，`position > queueThreshold`（默认 300）触发：
   - a. `tieredFallback` + `raceWithinTier`：**同档未尝试模型并发竞速**，先出内容者胜；
   - b. 同档耗尽 → **下一档第一个模型**；
   - c. 全部档位耗尽 → **兜底模型 `fallbackModel`（默认 glm-5）**；
   - d. 兜底也排队 → 放弃降级，**回到原模型继续等待**（不报错）；
   - 未开 `tieredFallback` 时走 legacy `mappings` 链（如 `claude-sonnet-4-6` → `glm-5.1` → `glm-5` → `qwen-3.6-plus`）。
   排队期间只发 SSE `ping`（不污染对话正文），并**提前发 `message_start`** 防 Claude Code 60s 超时。
4. **前置模型切换**：检测到图片输入且当前模型非多模态 → 切到同档/最近档多模态模型。
5. **完成后续写**：启发式截断检测（未闭合代码块/括号、尾随逗号/竖线/省略号、`max_tokens`、只有 reasoning 无正文）→ 自动续写，最多 5 次，带相似度防循环。

---

## 3. ZedeX 有、trae-cn-relay 没有的能力

| 能力 | 说明 | 本项目现状 |
|---|---|---|
| 排队 position 驱动的模型降级 | position>阈值触发同档竞速/按档降级/兜底 | 仅解析 position 并**作为正文文本**发出（`sse.py` 发 `排队中，当前位置：N`），无降级逻辑 |
| 模型分级降级体系 | `model-config.json` 5 档 + `fallback` 配置 + dashboard 在线改 | 只有硬编码模型别名表 + 端点级回退（raw→ide→remote）+ 429 账号轮换 |
| 同档并发竞速 | 多模型并发取先到者 | 无 |
| 流量日志 | 每请求 JSON：脱敏请求头、SSE chunk 时间线、token、耗时、排队位置；`/v1/dashboard/*` 查询与统计 API；活跃请求列表 | 只有消费记录（tokens/积分/状态），无原始流量回放 |
| 会话持久化 | node:sqlite `sessions`/`messages` 表、搜索、从某条消息截断、导出、`X-Session-Id` 自动落库、按会话覆盖采样参数 | 仅进程内 Responses 会话缓存（TTL 3600），无持久会话库 |
| Anthropic `/v1/messages` 入口 | Claude Code 直连：content_block 流、thinking 块、tool_use 块、ping | **完全没有**（只有 Chat Completions + Responses） |
| 截断自动续写 | 启发式检测 + 续写循环 + 相似度防循环 | 无 |
| 多模态自动切换 | 图片输入自动换多模态模型 | 直接丢弃（`[Unsupported image input omitted]`） |
| 6 格式 toolcall 文本解析 | 标签/XML/JSON 多格式容错 | 有基础解析与剥离，但格式覆盖较少 |
| 提前 `message_start` + ping | 排队期防客户端超时 | 无（无 Anthropic 入口） |
| 配置热加载 | `fs.watch` 监听 model-config/fallback 并热重载 | 模型列表有 TTL 缓存，配置改动需重启/接口触发 |
| 文件输出 | `save_to` 参数 + `/v1/chat/file` + `OUTPUT_SYNC_DIR` 同步 | 无 |
| 静态 config schema | schema 驱动 UI + 全局默认 + 会话级覆盖 | 有设置页，但无统一 schema 体系 |
| HTTP/SOCKS 代理 | 所有上游调用支持代理 agent | 依赖 httpx `trust_env`，无显式代理配置 |
| CN/SG/US 三版自动探测 | 双版本回退 + 手动 token 兜底 | 仅 CN 为主（多账号体系另算） |

---

## 4. ZedeX 缺少、trae-cn-relay 具备的能力

- **多账号体系**：无账号池/OAuth 网页登录/顺序与积分优先轮询/按账号模型开关/并发槽位与排队。
- **签到与积分**：无每日签到、9074 风控换设备 ID、积分账单查询与消费记录。
- **更多上游通道**：无 Remote/Web（`chat_sessions`+`events`）、Work Agent、CLI、TraeWork 原生桥接、raw v2 诊断通道；只有 IDE 三端点。
- **Responses API**：无 `/v1/responses`（Codex 的 `previous_response_id`、`function_call_output` 续轮等）。
- **原生 tools 协议**：IDE/Solo 端点不发 `tools` 字段，全部靠提示词注入，工具能力受模型文本遵循度限制。
- **思考强度映射**：无 `reasoning_effort`/`thinking.budget_tokens` → Trae `light/high/extra_high` 及按模型钳制。
- **1M Max 上下文模式**（`strategy=max`、独立 max 会话）。
- **流健壮性**：无 SSE 心跳、无累积快照去重、无"重复工具调用抑制"、无上游模型名校验（provider mismatch）。
- **账号级错误恢复**：429 只做同账号退避重试，不做账号轮换。
- **历史压缩与预算**：无按字符数的历史裁剪/工具 schema 压缩（relay 有 raw/remote 的 query 上限与压缩）。
- **运维形态**：无 Docker/打包发布/发布校验；控制台只有 dashboard 读侧。
- **链路诊断字段**：无 `fallback_reason` / `failed_endpoint` / `actual_endpoint` 级 trace 写入消费记录。

---

## 5. 值得移植到 trae-cn-relay 的点（按优先级）

1. **排队降级策略**：把 ZedeX 的 `queueThreshold` + 同档竞速 + 按档降级 + 兜底模型接到 relay 的 remote/ide 通道。relay 目前排队只能干等或等 429，对热门模型体验差距明显。落地方式：在 `accounts.json`/新配置文件里加 tier 表与 fallback 链，在解析 `request_wait_in_queue` 处触发。
2. **排队信息不再写进正文**：`sse.py` 现在把 `排队中，当前位置：N` 作为 `content` 发出，会污染调用方上下文（ZedeX 明确修过这个问题）。改为 SSE 注释心跳/ping 或仅在控制台与 usage 记录中暴露。
3. **截断自动续写**：检测未闭合代码块/括号、尾随 `, | ... \ // #`、`max_tokens`、只有 reasoning 无正文，并带相似度防循环。对 Codex/Agent 的长回答完整性价值最高，实现成本低（纯后处理 + 追加一轮）。
4. **流量日志与排障视图**：移植 traffic-logger 的形态——请求头脱敏、SSE chunk 时间线（含排队位置、timing_cost）、耗时与 token 落盘，并提供读取 API。relay 现有消费记录不足以定位"排队多久/卡在哪个事件"。
5. **Anthropic `/v1/messages` 入口**：Claude Code 兼容是当前最大生态缺口；relay 的 `sse.py` 工具管线已具备大部分能力，主要补 content_block 事件封装与 thinking 块。
6. **多模态检测自动切换**：图片输入当前被静默丢弃，至少应自动切多模态模型或明确报错。
7. **模型/降级配置外置 + 热加载 + 在线编辑**：把硬编码的 `MODEL_ALIASES`/档位迁到外部配置文件，配合控制台编辑与 `fs.watch` 热加载（relay 已有持久化设置页，可复用）。
8. **持久化会话**：Python 侧用 sqlite 实现 sessions/messages + 截断/导出；比进程内 Responses 缓存更适合多客户端与重启恢复。
9. **toolcall 文本解析容错**：对照 ZedeX 的 6 格式解析（属性式 XML、arg_key/arg_value、修尾逗号）补齐 relay 文本桥接的兜底分支。
10. **toolMap 别名映射**：若接入 Anthropic 入口，直接复用其 Claude Code 工具名 → relay 工具名的映射思路。

### 不建议移植

- `chatCompletion()` 把内部 options 原样 spread 进上游 body 的写法（易把本地字段泄漏给上游）。
- `agent.js` 那套未接线的本地工具执行器（relay 的调用方工具协议更完整）。
- 单 token 认证模型（relay 的多账号/签到/积分体系是其核心优势，不应回退）。

---

## 附：请求链路速查对照

| 维度 | ZedeX | trae-cn-relay |
|---|---|---|
| 语言/框架 | Node.js + Express | Python + FastAPI |
| 上游主通道 | `llm_utils_chat`（`function` 字段选择模式） | Remote `chat_sessions`（默认）/ Solo / IDE（`solo_work_lite`） |
| 端点降级 | llm_utils_chat → ide/v1/chat → create_agent_task | 选定端点失败 → 回落 Remote（账号级重试） |
| 模型降级 | 5 档 + 同档竞速 + 兜底模型（排队触发） | 无（仅账号轮换与端点回退） |
| tools 协议 | 提示词注入 + 文本解析（无原生字段） | 原生 tools 字段（IDE/Solo）+ 文本桥（Remote） |
| 排队事件 | 解析 position，触发降级，只发 ping | 解析 position，作为正文文本发出，无降级 |
| 客户端入口 | OpenAI Chat + Anthropic Messages | OpenAI Chat + Responses |
| 多账号 | 无 | 有（OAuth/轮询/签到/积分/并发槽） |
| 持久化 | SQLite 会话 + 流量日志文件 | 账号/设置/消费记录 JSON + 进程内 Responses 缓存 |
