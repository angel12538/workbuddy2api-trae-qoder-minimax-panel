# 风控规避作战手册（国内版 / 国际版 / 容器·数据中心）

> 基于 issue #10/#18/#38/#51/#57/#59 实测 + 官方客户端只读逆向（`app.asar`、`qoderclicn.exe`、
> `~/.qoder/.cache/endpoint-cache.json`）。目标：**降低被限/被排队的概率**，并让容器部署尽可能接近官方形态。

---

## 0. 先认清三条硬约束（否则所有努力都会跑偏）

1. **队列是模型级的**（`modelKey + queueType`），不是账号级。多账号只分摊额度/并发，**不增加队列容量**。
2. **数据中心 IP 无法用协议头伪装成住宅 IP**。上游若按 ASN/地理做风控，唯一解是**换出口**，不是换头。
3. **设备身份由官方 SDK 计算**（intl 走 `umid/sgsdk.dll` + `runtime-info.exe`，按账号）。
   我们派生的是「稳定的假设备」——能避免被判爬虫，但不等于官方客户端。

结论：**出口 > 身份 > 调度 > 请求整形**。顺序反了就是白忙。

---

## 1. 分层处方

### L0 出口（决定 70% 的结果）

| 场景 | 做法 |
|---|---|
| 国际版 + 容器/云主机 | **必须**经住宅/办公出口：住宅 SOCKS5/HTTP 代理，或「家里网关 + 隧道」 |
| 国际版 + 本机 | 直连即可，但**固定出口**（别频繁切节点） |
| 国内版 | 数据中心 IP 容忍度高，可直连；仍建议固定出口 |
| 多账号 | 每账号**粘一条出口**（account→proxy 固定映射），不要所有号共用一个 IP |

实现要点：
- 代理只走**进程环境变量**（#27：网关不读系统代理），每账号映射用 `QD_ACCOUNT_PROXY_<uid前缀>`（规划中）；
- 出口失败**才**换节点，换完保持粘性；
- 家庭网关方案：家里跑一个 SOCKS5（或 `ssh -R` 反向隧道）→ 云上网关连它，出口 IP = 家庭宽带。

### L1 身份（低成本、必须做对）

- 设备码/会话码**按账号稳定派生**（本仓已做），跨账号不共享；
- 容器里 `accounts/` 必须挂卷（`machine_identity.json` 缓存官方设备身份，重建不换，#18）；
- 固定容器 MAC（`mac_address`）或 `network_mode: host`；
- **不要**每请求随机 UA/设备码，**不要**频繁 `QD_MACHINE_IDENTITY_RESET=1`；
- 推理头对齐官方（本仓 v1.5.9+）：`cosy-version / cosy-clienttype / cosy-machineos /
  cosy-machinehostname`（仅 infer-sse）、`cosy-machineid / machinetoken / machinetype`。

### L2 调度（本仓已内置，可按需收紧）

| 机制 | 作用 | 旋钮 |
|---|---|---|
| 会话亲和 | 同一会话粘同一账号，避免「多号来回跳」 | 默认开 |
| 队列等待 | 10605 等待而非报错（对齐官方分钟级） | `QD_QUEUE_MAX_ROUNDS=6` / `QD_QUEUE_WAIT_BUDGET_SECS=600` / `QD_QUEUE_WAIT_JITTER=2` |
| 模型级冷却 | 429 只冷却该模型 | 自动 |
| 熔断/降权 | 连续 3 次硬失败才熔断（30min 起，封顶 6h） | 代码常量 |
| 单号并发上限 | 防止一个号被多客户端打爆（**规划中**） | `QD_MAX_CONCURRENT_PER_ACCOUNT` |
| 最小请求间隔 | 近似人类节奏，避免突发 | `QD_MIN_INTERVAL_MS`（**规划中**） |

### L3 请求整形

- **前缀稳定**（#48/#55）：`QD_TOOL_RESULT_SCOPE=tail`（默认）、Anthropic 侧 system 不合并动态内容；
- **结构化工具历史**（#49）：DeepSeek 已进白名单，减少复述与重试；
- **体积上限**（#59）：`QD_MAX_BODY_MB=29`，超限本地 413，不触发 Broken pipe 连锁；
- `max_tokens` 合理，别动辄 32k；
- 客户端 SDK 重试要带退避（≥ 1s，指数），别秒级重试。

### L4 观测（先量再调）

- `GET /health` → `risk` 字段：`queue / rate / rejected / version_gate` 计数 + 最近一次信号；
- `python scripts/risk_diag.py`：账号健康、旋钮、容器+intl 告警、日志信号；
- 面板：冷却/熔断/降权/软限流；`usage.jsonl` 的 `sys_src` / `n_msg_system` / `cached_tokens`。

### L5 应急

- 见到 `10605`：**等**（已自动），不要换号刷；
- 见到 `429`：等该模型冷却，或换模型；
- 见到 `403 + TOKEN_EXPIRE`：重新授权，不要重试；
- 见到 `430`：**客户端版本过低**，升级（硬门控，无法绕过）；
- 见到 `114`：试用账号受限，换正式账号。

---

## 2. 国内版 vs 国际版：处方不同

| 维度 | 国内版 (cn) | 国际版 (intl) |
|---|---|---|
| 出口要求 | 宽松，数据中心可接受 | **严格**，优先住宅/固定 IP |
| 推理端点 | `gateway.qoder.com.cn` | `api3.qoder.sh`（主）→ api2 → api1 |
| 安全推理 | 与推理同 host | **独立 `api2.qoder.sh`**（另一套限流） |
| 签到/活动 | 有接口 | 无（404） |
| 设备身份 | UMID 同机制 | UMID + 更严格的风控 |
| 容器部署 | 可 | **不建议直连**，需隧道 |

---

## 3. 容器 / 数据中心专项（重点）

### 3.1 必须做的 6 件事

1. `accounts/` 与 `usage/` **挂卷**（身份与缓存不随容器消亡）；
2. `mac_address` 固定（或 `network_mode: host`）；
3. `QD_ALLOW_DC_EGRESS=1` 显式确认（**规划中**：未确认时 intl 直接拒绝启动）；
4. intl 流量**必须**经住宅出口（见 3.2）；
5. 单容器**单账号池**，别把一个 accounts 目录复制到多台机器；
6. 打开 `risk_diag` 与 `/health` 的 risk 计数，观察 10605/429 比例。

### 3.2 三种把「数据中心」变成「住宅出口」的方案

**A. 住宅代理（最简单）**

```yaml
services:
  qoder-proxy:
    environment:
      HTTPS_PROXY: http://user:pass@residential-proxy:port
      HTTP_PROXY:  http://user:pass@residential-proxy:port
      QD_ALLOW_DC_EGRESS: "1"
```

要求：**粘性 IP**（session 保持），按账号分配不同 session（`user-session-<uid>:pass`），
不要轮换式代理（每请求换 IP 最容易被判异常）。

**B. 家庭网关 + 反向隧道（最稳）**

```
[家里]  ssh -R 1080:127.0.0.1:1080 user@vps   # 暴露本地 SOCKS5
[VPS]   HTTPS_PROXY=socks5h://127.0.0.1:1080   # 网关进程走这条
```

出口 IP = 家庭宽带；重启 VPS 不影响身份（身份在 `accounts/`）。

**C. 自建机房 + 家宽双线（成本最高）**

主走家庭出口，备用走机房（仅在家庭链路故障时切），切换要慢、要记录。

### 3.3 反面清单

- ❌ 容器里 intl 直连（#57 实测直接进排队）；
- ❌ 一次请求换一个代理节点；
- ❌ 多个账号同一 IP 同时高并发；
- ❌ 用「随机 UA/随机设备码」掩盖（等于告诉上游你是脚本）。

---

## 4. 分阶段落地

### P0（已实现，直接可用）

- [x] intl 出口顺序对齐官方（api3 → api2 → api1，`QD_INTL_GATEWAY` 可覆盖）；
- [x] 推理头补齐 `cosy-machineos` / `cosy-machinehostname`（`QD_COSY_OS_HEADERS=0` 可关）；
- [x] 排队等待分钟级 + 抖动（`QD_QUEUE_*`）；
- [x] 请求体积前置 413（`QD_MAX_BODY_MB`）；
- [x] `/health` 增加 `risk` 计数；`scripts/risk_diag.py` 只读诊断；
- [x] 容器 + intl 启动 WARN。

### P1（建议下一步做）

- [ ] **账号→出口粘性映射**：`QD_ACCOUNT_PROXY_<uid8>=socks5h://...`，失败才换；
- [ ] **单号并发 + 最小间隔**：`QD_MAX_CONCURRENT_PER_ACCOUNT`、`QD_MIN_INTERVAL_MS`（超限排队而非失败）；
- [ ] **队列状态轮询**：官方 `/api/v2/service/ask/queue/status`（按 `requestSetId/modelKey` 轮询，省去盲等）；
- [ ] **版本门**：`430` 时给出「升级客户端/升级网关」的明确提示（当前只透传错误）。

### P2（长期）

- [ ] 自适应节流：按 `risk` 计数自动降并发/切模型；
- [ ] 设备身份一致性审计（重启/重建后派生值比对）；
- [ ] 多出口健康检查与灰度切换。

---

## 5. 验收指标

| 指标 | 目标 | 采集 |
|---|---|---|
| 10605 排队率 | < 5% 请求 | `/health.risk.queue` |
| 429 频控率 | < 2% | `/health.risk.rate` |
| 403/失效 | ≈ 0 | `/health.risk.rejected` |
| 前缀缓存命中 | > 80% | `usage.jsonl.cached_tokens / prompt_tokens` |
| 设备码漂移 | 0（重启后不变） | `machine_identity.json` + 派生值 |
| 单号并发峰值 | ≤ 配置上限 | 日志/面板 |

---

## 6. 只做软件层（不碰 IP/隧道）时，我们能控制什么

> 前提：出口 IP 与隧道由部署方决定，网关无法改变。以下全部是**网关内部**能做的事。

### 6.1 已落地

| 措施 | 作用 | 开关 |
|---|---|---|
| **流量守卫**（原团队闸门扩展到所有账号） | 单号并发 1、最小间隔 400ms，避免「自己把号打爆」被限 | `QD_TRAFFIC_GUARD=on` / `QD_TEAM_MAX_CONCURRENCY` / `QD_TEAM_MIN_GAP_MS` |
| **排队不换号** | 10605 是模型级队列，换号无效；不再解绑会话亲和，避免「同会话在多号间跳」的异常画像 | 自动 |
| **排队等待** | 分钟级预算 + 抖动，表现为「慢」而不是「403」 | `QD_QUEUE_MAX_ROUNDS` / `QD_QUEUE_WAIT_BUDGET_SECS` / `QD_QUEUE_WAIT_JITTER` |
| **推理头对齐官方** | `cosy-machineos` / `cosy-machinehostname`（仅 infer-sse），避免形态差异被标记 | `QD_COSY_OS_HEADERS=0` 可关 |
| **体积前置 413** | 避免 30MB 被 RST 后连锁熔断 | `QD_MAX_BODY_MB` |
| **风控遥测** | `/health.risk` 计数 + 最近信号，先量再调 | 自动 |
| **430 版本门提示** | 明确告知「升级才能继续」，不误判为风控 | 自动 |
| **前缀稳定 / 结构化历史** | 少发字节、少复述，降低单请求压力 | `QD_TOOL_RESULT_SCOPE` / `QD_STRUCTURED_TOOL_HISTORY` |

### 6.2 建议下一步（仍在软件层）

1. **请求排队而不是并发**：超过单号并发上限时**排队等待**（当前是 30s 后 fail-open 放行），
   进一步降低突发；
2. **自适应节流**：`/health.risk` 里 `queue/rate` 在窗口内升高 → 自动把并发降到 1、
   间隔抬到 1~2s，冷却后恢复；
3. **模型级分流**：同模型排队率过高时，若客户端用 `auto`，临时改投同区其它模型；
4. **账号节奏错峰**：多账号轮换加「最近最少使用」权重，避免同一秒多个号一起发；
5. **队列状态轮询**（官方协议）：`/api/v2/service/ask/queue/status` 精确等待，减少无效重发；
6. **失败语义收敛**：已做 #37（参数非法 fail-closed）、#9/#11（截断回声吞掉），继续避免
   「重试导致重复执行」。

### 6.3 软件层做不到的（只能靠部署方）

- 数据中心 IP 被判风险 → 只能换出口；
- 账号被上游封禁/降级 → 只能换号或申诉；
- 团队席位的并发硬限 → 只能加席位；
- 官方版本门（430）→ 只能升级。

