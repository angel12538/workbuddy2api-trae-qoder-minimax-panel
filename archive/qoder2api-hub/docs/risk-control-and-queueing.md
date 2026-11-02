# Qoder 国际版风控与「排队」机制观察笔记

> 结论先行：**多账号解决的是「额度/并发分摊」，解决不了「模型级队列」**。
> 10605 / RESOURCE_LIMIT 是**模型容量**信号（不是封号），同一模型的所有账号共享同一个池；
> 真正能降低被排队概率的是**出口 IP 与请求整形**，不是换号。
>
> 本文基于本仓代码 + issue #10 / #18 / #38 / #51 / #57 的实测整理，不含任何账号凭证。

---

## 1. 上游看到的「你」有哪些维度

| 维度 | 具体内容 | 本仓现状 |
|---|---|---|
| 客户端指纹 | `cosy-machineid` / `cosy-machinetoken` / `cosy-machinetype` / `cosy-version` / `cosy-clienttype`、`User-Agent` | `qoder_sign` 生成 COSY 签名头；`Account.headers()` 带 `X-Machine-ID`/`X-Session-ID`/`X-Request-ID`；`desktop_headers()` 走桌面端形态 |
| 设备身份 | 官方组件输出的物理机身份（容器里是「容器级」） | `accounts/machine_identity.json` 落盘缓存（issue #18），重建/升级不换 |
| 账号 | access token、plan、团队/组织、realm | 按账号隔离，token 过期自动禁用 |
| 网络 | 出口 IP、ASN（住宅 vs 数据中心）、地理区域 | 走进程环境变量的代理；容器出口 = 数据中心 IP |
| 行为 | 并发、频率、模型分布、上下文长度、请求体积 | 账号亲和 / 轮换 / 冷却 / 熔断；前缀稳定与体积上限（#48/#55/#59） |

**关键点**：设备身份按**账号 UID 稳定派生**（`qoder_fingerprint.derive_id`），所以多账号之间天然不共享 machineid——
这是「多号不被关联成同一台机器」的基础。**不要**为了"躲风控"去每请求随机换 ID，那会把「稳定设备」变成「异常设备」。

---

## 2. 上游的处置等级（按严重度）

| 信号 | 含义 | 处置 | 本仓处理 |
|---|---|---|---|
| `10605` + `isQueued:true` + `retryAfterSeconds` + `subCode:RESOURCE_LIMIT` | **模型级容量排队**（不是账号问题） | 等 N 秒再来 | HTTP 层 + **信封层**都按 retryAfterSeconds 等待重试（#38 / #51 后续） |
| `403` 队列满 / Token 被拒 | 排队或凭证问题 | 冷却换号 | 401/403 冷却并轮换；TOKEN_EXPIRE 直接停用账号 |
| `429` | 模型级频控 | 冷却该模型 | `model_cooldowns`（按模型粒度，不拖垮整号） |
| `418` / `provider_error` | 瞬态上游故障 | 重试 | 同号快速重试 2 次 → 短冷却换号 |
| 内容审核（DataInspectionFailed） | 确定性拒绝 | 不重试 | 直接返回可读原因 |
| 活动平台「机器头全套」 | 被判定非官方客户端 | 隐藏可领取活动 | 派生身份时**不发** `cosy-machine*` 六头（issue #10） |

**注意**：`10605` 是**模型维度**的。也就是说：
- 换账号 **不会**让你绕过队列（大家排同一个模型的队）；
- 换**模型**（或错峰）才有用；
- 排队时最糟的做法是「快速失败 + 客户端立刻重试」——那只会放大失败率。

---

## 3. 为什么「多账号反代」会排队

1. **模型级队列**：`dfmodel`（DeepSeek-Flash）等在高峰有队列，`queueType:p3`、`serviceAvailable:false`。多号只是让你有更多「排队资格」，不是更多「队列容量」。
2. **同 IP 关联**：多个账号从同一个出口 IP 高频请求，容易被归到同一"使用群体"，触发更严的限流。
3. **数据中心 IP**：容器/云主机出口更容易被判定为自动化流量（issue #57 的实测：容器反代国际版 → 聊天变排队）。
4. **本地冷却叠加**：账号冷却（`cooldown_until`）、模型冷却（`model_cooldowns`）、硬错误熔断（`breaker_until`，连续 3 次硬失败 → 30 分钟起，封顶 6 小时）、未知错误降权（`degrade_until`）——这些会让人误以为"上游在排队"，其实是本地把号停了。
5. **并发超限**：团队/企业席位常有并发上限；同一账号被多个客户端并发使用会撞限流。

---

## 4. 避免/缓解办法（按性价比排序）

### 4.1 出口网络（最高性价比）

- **国际版不要跑在数据中心 IP 上**（容器/云主机）。用住宅/办公出口，或给 intl 单独走一条住宅代理：
  ```bash
  # 只给网关进程设代理（#27：只认环境变量，不读系统代理）
  HTTPS_PROXY=http://住宅代理:端口 HTTP_PROXY=http://住宅代理:端口 python qoder_proxy.py --port 8790
  ```
- **固定出口 IP**：不要频繁切换代理节点。出口一变，设备/账号的行为画像就断，容易重新被判风险。
- **cn 与 intl 分开出口**：两个区域的风控策略不同，混在一个出口上更容易被关联。

### 4.2 设备指纹（低成本、必须做对）

- 保持**按账号稳定派生**（默认行为，无需配置）；
- 容器部署时**挂载 `accounts/`**：`machine_identity.json` 会缓存官方组件输出的身份，重建/升级不换（issue #18）；
- 想更稳可再固定容器 MAC（`mac_address`），或 `network_mode: host`；
- **不要**设 `QD_MACHINE_IDENTITY_RESET=1` 频繁"换身份"，也不要多个账号共用同一份 `accounts/`（那等于共用设备）。

### 4.3 调度整形（本仓已内置，可调）

| 机制 | 作用 | 相关开关 |
|---|---|---|
| 账号亲和（`SessionAffinity`） | 同一会话粘同一账号，减少"多号来回跳"的异常画像 | 默认开启 |
| 队列等待 | 10605 排队时等待而非报错 | `QD_QUEUE_MAX_ROUNDS`（默认 3）、`QD_QUEUE_WAIT_MAX_SECS`（180）、`QD_QUEUE_WAIT_BUDGET_SECS`（300） |
| 模型级冷却 | 429 只冷却该模型，不拖垮整号 | 自动 |
| 熔断/降权 | 连续硬失败才熔断（3 次），避免一个坏号拖垮请求 | `BREAKER_*`（代码常量） |
| 短冷却等待 | 冷却窗口 ≤10s 时等待而不是换号 | 自动 |

**战术建议**：高峰期把流量**分散到不同模型**（Auto 之外显式指定可用模型），避免所有并发都打在同一个排队模型上；客户端侧把并发降到 1–2。

### 4.4 请求整形（减少"看起来像滥用"）

- **上下文别无限增长**：前缀稳定（#48/#55）+ 结构化工具历史（#49）能显著降低每轮重发的字节；
- **控制单请求体积**：图片数量/尺寸（#59 已加 29MB 前置 413），长工具结果按需截断（`QD_TOOL_RESULT_SCOPE`）；
- **`max_tokens` 合理**：超长输出会拉长单请求占用，放大排队概率；
- **减少无意义重试**：客户端 SDK 的重试要加退避，别 1 秒一次。

### 4.5 观测（先量再调）

- 面板账号行：`冷却 / 熔断 / 降权 / 软限流次数`；
- 日志关键字：`10605`、`isQueued`、`RESOURCE_LIMIT`、`429`、`TOKEN_EXPIRE`、`queued on`；
- 诊断脚本：`python scripts/risk_diag.py`（只读，不发上游请求）。

---

## 5. 反模式（会加速被限，别做）

- ❌ 每请求随机 `UA` / 设备码 / `X-Machine-ID` —— 稳定设备突然变"多台设备"，风险评分上升；
- ❌ 无脑换号重试 —— 队列是模型级的，换号无效，还会把多个号一起打进冷却；
- ❌ 同一账号同时挂多个客户端 / 多个 IP —— 并发与异地同时在线是两个高危信号；
- ❌ 排队失败后秒级重试 —— 直接放大队列压力，`retryAfterSeconds` 就是让你等；
- ❌ 把 `accounts/` 打包分发 —— 等于把同一台"设备"复制到多台机器。

---

## 6. 一页速查

```
排队(10605) ──> 等 retryAfterSeconds（本仓已自动）──> 仍失败 ──> 换模型 / 错峰
429      ──> 该模型冷却 ──> 换模型或等待
403+TOKEN_EXPIRE ──> 账号失效 ──> 重新授权
容器+intl ──> 启动 WARN ──> 换住宅出口或改 cn
多账号   ──> 只分摊额度/并发，不增加队列容量
```
---

## 7. 官方客户端实测（逆向 `qoderclicn.exe` + `app.asar`）

> 方法：本机安装的 **Qoder CN 桌面端**（`AppData/Local/Programs/Qoder CN`）+ CLI core
> `%USERPROFILE%\.qodersec\bin\qoderclicn.exe`，只读字符串常量与调用链，未修改任何文件。

### 7.1 排队是**独立的轮询协议**，不是「失败后重试」

```
GET /api/v2/service/ask/queue/status?requestSetId=<id>&modelKey=<key>&queueType=<type>
```

- 首次请求被拒（`10605` / `isQueued:true`）后，客户端**不结束请求**，进入轮询循环；
- 每轮先 `sleep(retryAfterSeconds)`（取自错误体 `queue.retryAfterSeconds`），再查一次状态；
- `isQueued === false` → 继续真正的推理请求；
- `serviceAvailable === false` → **继续轮询**（暂时无容量，不放弃）；
- 连续 **3 次**轮询失败 → 判致命（`Ok0=3`）；
- **总等待上限默认 60 分钟**（`3600000ms`），可用 `QODER_MODEL_QUEUE_MAX_WAIT_MS` 覆盖；
- 状态以 `system / model_queue_status` 事件流式回给客户端（`queue_count`、`wait_time_ms`、
  `queue_wait_elapsed_ms`、`queue_max_wait_ms`、`service_available`），UI 显示「模型负载较高」，
  给 **切换模型分级 / 重试** 两个动作。

**结论**：官方对排队是**分钟级耐心等待**，不是秒级失败。我们此前 300s 预算偏短。

### 7.2 身份头（官方实际发送）

| 场景 | 头 |
|---|---|
| 推理 SSE（`infer-sse`） | `Cosy-Version`、`Cosy-ClientType`、`Cosy-MachineOS`、**`Cosy-MachineHostname`（仅此场景）** |
| 中心/反馈 API | `X-Request-Id`、`X-IDE-Platform: qoder_cli`、`X-Version`、`X-Machine-OS`、`X-Machine-Id`、`X-Machine-Type`、`X-Machine-Code` |

本仓 COSY 头已覆盖 `cosy-machineid / machinetoken / machinetype / date / key / user / version / clienttype`，
与官方推理路径一致；差别只在 `Cosy-MachineHostname` 与少数 trace 头。

### 7.3 设备身份由原生 SDK 计算

- `resources/umid/` 内有 `sgsdk.dll` + `runtime-info.exe`（阿里 UMID 设备指纹）；
- 主进程按**账号**调用 `runtime-info.exe --account-stdin` 计算身份，函数名里带
  `NATIVE_RISK_IDENTITY_ABORTED` —— 即「原生风险身份」；
- 另有 `auth.machine-id`（本地持久化 UUID）作为机器标识。

**含义**：设备身份是**每账号一份、由官方 SDK 计算**。本仓派生稳定身份在「不被判成爬虫」这层有效，
但**不会**让上游认为你是官方客户端；行为异常（高频、多 IP、超大请求）仍会被风控。

### 7.4 客户端会专门渲染的错误码

`103 / 105 / 110 / 112 / 114 / 115 / 116 / 117 / 118 / 119 / 122 / 406 / 430 / 10605 / 80411 / 80412 / 100400 / 100401 / 100403`

与我们直接相关的：

- `10605` → 模型负载较高（排队）；
- `114` → 试用账户使用受限；
- `430` → **版本过低**（要求升级客户端）；
- `100401 / 100403` → 鉴权/权限拒绝。

### 7.5 重试策略（官方）

底层 HTTP（undici RetryHandler）对 `429 / 500 / 502 / 503 / 504` 与
`ECONNRESET / EPIPE / UND_ERR_SOCKET` 自动重试；排队走 7.1 的轮询协议。

### 7.6 由此得到的两个直接改进

1. **等待预算对齐官方**：网关默认从「3 轮 × 30s ≈ 90s」提升到**分钟级**
   （`QD_QUEUE_MAX_ROUNDS` 默认 6、`QD_QUEUE_WAIT_BUDGET_SECS` 默认 600），并加 0~2s 抖动，
   避免多客户端同时唤醒打同一队列；
2. **可见性**：等待期间日志明确写「第 N 轮 / 已等 Xs」，客户端侧表现为「慢」而不是「403」。

---

## 8. 国际版（intl）与国内版的差异（实测）

数据来自本机 intl 客户端（Qoder 0.4.8）的 endpoint 缓存：
`~/.qoder/.cache/endpoint-cache.json` 与 `qoder-client-endpoint-cache.json`。

```json
{
  "inference":          "https://api3.qoder.sh",
  "securityInference":  "https://api2.qoder.sh",
  "center":             "https://center.qoder.sh",
  "openapi":            "https://openapi.qoder.sh"
}
```

### 8.1 intl 把「推理」和「安全推理」拆成两个集群

- 国内版：`securityInference` 默认与 `inference` **同一个 host**；
- 国际版：推理走 **api3**，**安全推理单独走 api2**。

这是 intl 独有的风险层：一部分推理类请求（安全/守卫类判定）打到 **api2 专用集群**，
与正常对话的 api3 分开限流。**用 api1 做主力是旧配置**——官方 0.4.8 候选首位是 api3，
本机实测 api1 常年超时（api1 直连 6s 超时，api2/api3 均正常响应 404）。

> 本仓已按官方顺序对齐：intl 主 `api3`，备 `api2` / `api1`；可用 `QD_INTL_GATEWAY` 覆盖。

### 8.2 endpoint 可被服务端「选举」覆盖

客户端按用途（`inference` / `securityInference` / `center` / `openapi` / `webSearch`）解析地址，
且支持 `elected*` 覆盖（`electedSecurityInferEndpoint` 等）。含义：上游可按账号/区域/A-B
动态把客户端导向不同集群——「同版本同网络、不同账号表现不同」是可能的。

### 8.3 intl 的其他已知差异

| 项 | intl | cn |
|---|---|---|
| 签到接口 `/sash/api/v1/me/daily-check-in/*` | 404（不存在） | 可用 |
| 团队共享额度 | `orgResourcePackage`（cap/available） | 同 |
| openapi 可达性 | 受 GA/出口影响大（直连偶发超时） | 稳定 |
| 容器出口 | 更容易被风控/排队（#57） | 相对宽松 |

### 8.4 实践结论

1. **intl 用 api3 主力**（已对齐），传输失败自动切 api2/api1；
2. intl 更依赖出口质量：住宅/固定 IP 优先，容器直连最容易排队；
3. 安全推理(api2) 与对话(api3) 分限流——一侧被限不代表另一侧；
4. 队列仍是**模型级**：换区不解决排队。
