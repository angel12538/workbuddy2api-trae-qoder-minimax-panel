# 实现原理

这一篇写给想了解网关内部机制、或者要排查疑难问题的读者。日常使用不需要看。

- [请求链路](#请求链路)
- [设备指纹](#设备指纹)
- [账号调度与容错](#账号调度与容错)
- [流式传输](#流式传输)
- [工具调用](#工具调用)
- [前缀缓存](#前缀缓存)

## 请求链路

```
客户端请求（OpenAI / Responses / Anthropic）
  → 转换为统一的 Chat 请求，校验模型闸门
  → build_qoder_body()  套用官方 baseprompt 模板，压平会话（system / 工具 / 参数）
  → qoder_encode()      Qoder 自定义 Base64（三段轮转 + 字母表映射，'=' → '$'）
  → COSY 签名           RSA-1024 包裹 AES-128 会话密钥；
                        Bearer = COSY.{payload}.{md5(payload\ncosyKey\ndate\nbody\npath)}
  → POST {gateway}/algo/api/v2/service/pro/sse/agent_chat_generation?…&Encode=1
  → 解开 SSE 信封        {"headers", "body", "statusCodeValue"} → 内层 OpenAI chunk
  → 转换回客户端协议，以 SSE 或 JSON 返回
```

协议常量与官方客户端保持一致：

| 项 | 值 |
|---|---|
| `cosy-version` | `1.1.75`，可用 `QD_COSY_VERSION` 覆盖 |
| 推理 / 模型列表的 User-Agent | `undici`（官方 CLI 不设 UA，由其 HTTP 库补默认值） |
| 桌面客户端版本（签到请求头中使用） | `0.4.8`，可用 `QD_DESKTOP_VERSION` 覆盖 |
| 签到 / 活动请求头 | 与官方桌面端一致：`Accept: application/json`、`User-Agent: Qoder`、`Authorization`、`Cosy-ClientType: 10`、`Cosy-Version`，有真实身份时再带 `Cosy-Machine*`（主机名取本机 hostname）；领取请求另带 `Content-Type` |
| 国内版推理主机 | `gateway.qoder.com.cn` |
| 国际版推理主机 | `api3.qoder.sh`，传输失败时依次切换到 `api2` / `api1`（与官方客户端的候选顺序一致，可用 `QD_INTL_GATEWAY` 覆盖） |
| 账号接口的 User-Agent | 国内版 `QoderWork/1.1.75`，国际版 `Qoder/1.1.75` |
| `X-Request-ID` | 每个请求纯随机的 32 位 hex |

所有密码学算法（AES-128/256、GCM、RSA-PKCS1v15、DPAPI、QMC 解密）都在 `qoder_sign.py` 中用纯 Python 实现，因此在 alpine 容器中也不需要额外依赖。

## 设备指纹

网关使用两套设备身份，分别服务于不同链路：

| 用途 | 来源 | 特点 |
|---|---|---|
| 推理请求 | `derive_id(uid, salt)`：对账号 UID 加盐后取 MD5 派生，输出 UUIDv4 形态（与官方 machineId 同形；`QD_DEVICE_ID_LEGACY=1` 保持旧的 32 位 hex） | 同一账号永远相同，不同账号互相独立 |
| 签到 / 活动 | 官方 UMID 组件（`runtime-info`）输出的真实机器身份 | 机器级：同一台机器上的所有账号共用一个 |

获取真实机器身份：

- **Windows**：优先调用桌面客户端自带的 `runtime-info.exe`；没装桌面客户端时，可用 `_install_umid.py` 提取 `runtime-info.exe` + `sgsdk.dll`
- **Linux / macOS**：用 `_install_umid.py` 从官方 npm 包中提取组件（Docker 镜像构建时已经提取好）

首次获取时会多次采样，取多数结果，避免身份落在官方虚拟机检测结果偶尔跳变的少数分支上；结果缓存在 `accounts/machine_identity.json`。如果服务端不认可当前身份，网关会清除组件的种子文件，重新获取一个新身份。

签到 / 活动链路只有拿到真实身份时，才会发送 `Cosy-MachineToken` 等机器身份请求头。实测发送派生的假身份，反而会让「每日领取」活动被整条过滤掉。

推理与模型列表请求都携带 `cosy-machineos`，推理（infer-sse）请求另带 `cosy-machinehostname`，与官方客户端一致。其中主机名按账号稳定派生，不同账号互不相同；`QD_COSY_OS_HEADERS=0` 可以关闭。

## 账号调度与容错

**选号：** 从可用账号中轮询选择。同一段对话通过前缀亲和固定到同一个账号，以命中上游的前缀缓存。有限额护栏时，跳过已暂停的账号；开启临期优先后，余额即将到期的账号优先派发。开启[流量守卫](configuration.md#流量守卫)后，每个账号的并发数和请求节奏受到限制，超出的请求排队等待。

**错误处理：**

| 情况 | 处理 |
|---|---|
| 编码后的请求体超过 `QD_MAX_BODY_MB` | 本地直接返回 413，不发请求、不换号、不冷却账号 |
| 连接层瞬时错误（TLS 抖动、连接重置） | 同一账号重试 2 次（间隔 1s / 2s） |
| HTTP 200 已建立流，但 SSE 信封里是 418 / 5xx | 只要还没向客户端发出任何字节，就重新请求上游，最多 2 次 |
| 上游排队（403 + `isQueued` / 10605 / `RESOURCE_LIMIT`） | 按 `retryAfterSeconds` 等待（另加 0–2 秒抖动）后重试，默认最多 6 轮、共 10 分钟（`QD_QUEUE_*`）。排队是模型级的，不解绑会话亲和 |
| 重试后仍然失败 | 账号冷却 15 秒（单账号池为 3 秒）后换号。冷却剩余不超过 10 秒时，后续请求会等待，而不是直接报错 |
| 账号级 429 | 指数退避：10 分钟起，每次翻倍，最长 2 小时 |
| 硬错误 / 未知错误 | 熔断（连续 3 次起，30 分钟起，最长 6 小时）/ 降权（10 分钟起）。单账号池不触发 |
| 参数错误、内容安全审核拒绝 | 同一账号不重试；冷却该账号 60 秒后换号，都失败时返回中文说明 |
| 430 版本过低 | 返回「客户端 / 网关版本过低，请升级」 |
| 工具调用参数非法 | 不重试，不冷却账号，返回协议级错误（见下文） |
| 会话失效（`TOKEN_EXPIRE` / `12153`） | 停用账号，标记为需要重新登录 |

排队、限流、拒绝、版本门等信号的累计次数和最近一次详情，可以在 `/health` 的 `risk` 字段中查看。

## 流式传输

- 流式响应使用 HTTP/1.1 `Transfer-Encoding: chunked`，连接保持 keep-alive，可以复用
- 上游首字延迟可能长达 40–70 秒（高思考档位加长上下文）。这期间网关每 5 秒发送一个 SSE 注释帧 `: ping`，防止客户端或中间代理因空闲超时断开
- 上游有时会把错误藏在 HTTP 200 的信封里。网关按类别统计这类错误，并在每个类别首次出现时打印一条 WARN；计数可以在 `/usage/perf` 的 `inner_errors` 字段中查看

## 工具调用

**结构化历史：** 在白名单模型（国内版的 Qwen / GLM / DeepSeek）上，工具调用历史以结构化的 `tool_calls` / `tool` 消息发给上游。其他模型则把历史序列化成文本标记（`[assistant 请求调用工具]`、`[工具结果<name>]`）。只要有一个 `tool_call_id` 不完整，整个请求就退回文本化方式。

**泄漏回读：** 模型在长会话中有时会照着文本标记的格式，把工具调用写成普通正文。网关在流式、非流式、Responses、Anthropic 四条链路上统一处理这种情况：

| 正文形态 | 处理 |
|---|---|
| 完整的标记 + JSON 数组，且工具名在本次请求声明的工具中 | 还原成结构化的 `tool_calls`，`finish_reason=tool_calls`。标记的括号写法有变体（缺尾括号、多一层括号）时同样识别；标记前面同一帧里的正文照常输出 |
| DeepSeek 原生 DSML（`<\|DSML\|invoke …>`） | 解析后还原成 `tool_calls` |
| 内层 `arguments` 的引号没有转义，导致外层数组不是合法 JSON | 补齐转义后，按同一套严格校验重建，成功则还原成 `tool_calls` |
| 被截断、无法还原的回声 | 按严格条件吞掉，不把内部标记透给用户 |
| 数组结构合法，但内层参数非法 | 协议级失败：流式发送 error 帧，非流式返回 502，Responses 发 `response.failed`，Anthropic 发 `event: error`。不会伪装成正常结束 |
| 普通正文，包括讨论这些标记的正文 | 原样输出，不会吞掉正文 |

流式输出时，疑似标记的片段会先暂存，跨帧判定后再决定输出。一旦判定不是标记，暂存的内容立即原样补发。

**tool_call.id 补全：** 上游偶尔会发出缺少 id 的增量帧。网关会补上 `call_` 前缀的 id，并按 index 记住它，保证同一个调用分多帧时 id 不变。

## 前缀缓存

上游按账号缓存请求前缀。网关有几处设计专门保证前缀在每一轮之间保持字节级稳定：

- **会话亲和**：同一段对话固定使用同一个账号
- **历史工具结果截短**：默认只完整保留最后 1 条工具结果，更早的结果头尾各保留 2000 字符。一条结果一旦被截短，之后每一轮生成的字节都完全相同；省略标记是固定字符串，不含字数
- **Anthropic 的 system**：`messages` 内每轮都会变化的 system 内容原位改成 user 消息，顶层 system 保持不变

每次请求的 `sys_src` 和 `n_msg_system` 记录在 `usage.jsonl` 中，便于排查缓存命中率的问题。
