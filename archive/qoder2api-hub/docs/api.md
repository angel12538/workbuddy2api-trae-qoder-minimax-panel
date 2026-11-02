# 接口参考

## 鉴权方式

| 标记 | 含义 |
|---|---|
| 公开 | 不需要任何凭证 |
| Key | 需要 API Key（`Authorization: Bearer`、`x-api-key` 或 `?key=`）。没有配置任何 Key 时也可以访问。已登录的面板会话同样可以访问 |
| 面板 | 需要面板会话：先调用 `POST /panel/login` 拿到 token，之后在请求头 `X-Panel-Token` 中带上 |

## 推理

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/v1/chat/completions` | Key | OpenAI Chat Completions |
| POST | `/v1/responses` | Key | OpenAI Responses API |
| POST | `/v1/messages` | Key | Anthropic Messages |
| POST | `/v1/messages/count_tokens` | Key | Token 数估算（本地计算，不请求上游） |
| GET | `/v1/models` | Key | 模型清单；`?merge_realms=1` 同时返回两个区域的模型 |
| GET | `/models/refresh`（或 `/v1/models/refresh`） | Key | 立即向上游重新获取模型清单，并更新磁盘缓存。区域的选择规则与推理请求相同 |

- 指定区域：`X-Realm: cn|intl` 请求头，或 `?realm=` 查询参数
- 实际走的区域会写进响应头 `X-Qoder-Realm`；如果发生了自动切换，切换原因写进 `X-Qoder-Realm-Reason`

## 计费模型自检（`POST /diag/model-billing`，面板鉴权）

上游可能**按另一个模型名记账**：网关身份发出的请求，上游在 chunk 里回报的 `model` 常常是 `auto`（实测 dfmodel / kmodel_latest 都是），而同一账号用官方 CLI 请求就会记成真实模型名。这影响的是**上游侧的使用明细/额度归属**，不是网关路由。

```bash
curl -s -X POST http://127.0.0.1:8790/diag/model-billing \
  -H "Content-Type: application/json" -H "X-Panel-Token: $TOKEN" \
  -d '{"model":"DeepSeek-Flash","realm":"cn"}'
```

返回 `{ok, model, realm, billed: [...], billed_mismatch, account, chunks, elapsed_ms}`；`billed_mismatch=true` 表示上游按别的名字记账。发的是最小请求（1 个 ping 词 + `max_tokens=16`），耗时通常 1–3 秒。

面板「模型库」每行的 **自检** 按钮就是调它。

## 输出上限（max_tokens）

官方模型清单里没有 `maxOutputTokens`，但上游对超限请求会回 `400 invalid_parameter_error: Range of max_tokens should be [1, 131072]`。网关把这行错误当成**学习信号**：

- 解析出 `N` 后按 `(区域, 模型)` 记住并落盘到 `usage/max-output-limits.json`（7 天 TTL）；
- 之后的请求若 `max_tokens`（含 `/v1/responses` 的 `max_output_tokens`）超过 `N`，**自动压到 `N`** 并打一条 WARN，不再让客户端吃 400；
- `/v1/models` 会补上 `max_output_tokens` / `max_completion_tokens`，客户端可以自己先算。

也就是说：**第一次**超限仍会看到上游那条 400（网关要靠它才知道上限），之后同一模型就不会了。

## 探活与状态

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/ping`、`/healthz`、`/livez`、`/readyz` | 公开 | 返回纯文本 `pong`，不检查账号池 |
| GET | `/health` | 公开 | 版本、当前区域、账号数、风控信号、运行环境自检；带 Key 访问时额外返回当前账号信息 |
| GET | `/realm` | 公开 | 当前全局出口，以及自动选区是否开启 |
| GET | `/credits/summary` | Key | 各账号余额汇总，只返回本地记录的数值，**不请求上游**，适合其他系统轮询 |

`/health` 中用于排查的两个字段（都不含凭证）：

| 字段 | 内容 |
|---|---|
| `risk` | 网关启动以来收到的风控类信号次数：`queue`（排队）、`rate`（429 限流）、`rejected`（403 拒绝）、`version_gate`（430 版本过低），以及最近一次信号的详情 |
| `env` | 运行环境自检：`container`（是否在容器中）、`proxy`（是否配置了出口代理）、`accounts_writable`（`accounts/` 是否可写）、`identity_cached`（机器身份是否已缓存）、`clock_skew_s`（与上游的时钟偏差，秒）、`traffic_guard`（流量守卫模式） |

`/credits/summary` 返回的结构：

```json
{
  "by_realm": {"cn": {}, "intl": {}},
  "totals": {"remain": 0, "accounts": 0},
  "accounts": [
    {"uid": "", "nickname": "", "realm": "",
     "credits_remain": 0, "credits_used": 0, "credits_size": 0, "registered_at": 0}
  ],
  "note": ""
}
```

返回的是上一次记录时的余额，不是实时数值。从未记录过余额的账号也会列出，但相关字段为 `null`，并且不计入汇总。

## 看板与管理

以下端点供看板使用，均需要**面板**鉴权，另有说明的除外。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | Web 看板（公开） |
| POST | `/panel/login`、`/panel/logout`、`/panel/password` | 登录、登出、修改面板密码（公开） |
| GET / POST | `/settings`、`/settings/save` | 读取 / 保存设置（API Key、限额、模型闸门等） |
| GET | `/settings/reveal?id=` | 读取某个 Key 的明文（面板还在用默认密码时返回 403） |
| POST | `/realm` | 切换全局出口（有 Key 即可） |
| GET | `/logs`、`/logs/export`；POST `/logs/clear` | 运行日志 |
| GET | `/update/check` | 检查 GitHub 上是否有新版本 |
| GET | `/diag/vm` | 本机虚拟化检测 |

### 账号

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/accounts` | 账号列表 |
| POST | `/accounts/login/start`、GET `/accounts/login/poll`、POST `/accounts/login/cancel` | OAuth 设备授权 |
| POST | `/accounts/import/pat` | 导入 PAT |
| POST | `/accounts/import/desktop` | 请求体为 `{}` 时只扫描本机凭证；带 `{"path": ...}` 时导入指定凭证；带 `{"all": true}` 时导入全部 |
| GET | `/accounts/export`；POST `/accounts/import` | JSON 导出 / 导入 |
| POST | `/accounts/refresh`、`/accounts/test` | 刷新 Token、测试账号 |
| POST | `/accounts/set`、`/accounts/set-all`、`/accounts/delete` | 修改、批量修改、删除账号 |
| GET / POST | `/accounts/credits` | 逐个账号**向上游**查询余额（会请求上游，不要用来轮询） |
| POST | `/accounts/checkin` | 每日签到（单个或全部账号） |

### 签到、调度与用量

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/tasks` | 签到状态、活动、福利包资格、额度快照；`?realm=` 只看某个区域 |
| POST | `/tasks/run` | 批量签到并领取活动奖励 |
| POST | `/tasks/travel` | 批量领取 Pro 福利包 |
| GET | `/scheduler` | 调度器状态与排程日志 |
| POST | `/scheduler/trigger`、`/scheduler/toggle` | 立即执行一次巡检；开关调度器 |
| GET | `/usage`、`/usage/recent` | 用量汇总、最近请求 |
| GET | `/usage/analytics`、`/usage/by-account`、`/usage/by-key`、`/usage/perf` | 用量透视、性能指标。支持 `?range=day\|week\|month\|all\|custom&since=&until=` |
| GET | `/usage/timeseries` | 用量时间序列 |
| GET | `/usage/daily` | 每日用量，`?days=7\|30&group=model\|realm\|key\|account` |

### 智能体配置

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/agents` | 面板 | 本机客户端检测结果与接入状态 |
| GET | `/agents/models` | 面板 | 用于预填客户端配置的模型表 |
| GET | `/agents/cli` | 公开 | 配置器 CLI 的源码，供远程机器下载 |
| POST | `/agents/apply`、`/agents/restore` | 面板，且只接受本机访问 | 写入客户端配置 / 还原到接入前 |


## 用量洞察（参考 wb 的三项能力）

| 端点 | 说明 | 数据来源 |
|---|---|---|
| `GET /pricing` | 参考价目表（USD / 1M tokens）+ 未定价模型清单 | `qoder_pricing.DEFAULT_PRICES` + `usage/pricing-overrides.json` |
| `GET /accounts/credits/grants` | 积分包 / 获取记录（面额、剩余、已用） | 最近一次积分快照（**不发上游请求**） |
| `GET /accounts/credits/usage` | 官方积分消耗：累计 / 单日峰值 / 近 365 天逐日消耗 | 上游 `credits-summary` + `credits-heatmap`（0.4.3 客户端接口），每账号缓存 5 分钟；`?uid=` / `?realm=` 可过滤 |
| `GET /usage/remaining` | 每账号剩余积分 + 近 24h 消耗 + 预计可用天数 | `usage.jsonl` + 账号积分 |
| `GET /accounts/activity` | 签到 / 领取动作流水（30 天保留） | `usage/activity.jsonl` |

### 费用估算口径

- 输入按「缓存命中 / 未命中」两档、输出单独一档，单位 USD / 1M tokens；
- 只用于「这些 token 放在 OpenRouter 上大概值多少钱」的参考，**不能用于结算**；
- 未收录模型**不计价**（面板显示 `—`，绝不猜数字），可在 `usage/pricing-overrides.json` 覆盖：

```json
{"dfmodel": {"in": 0.28, "cached": 0.028, "out": 0.42},
 "my-custom-model": [0.5, 0.1, 2.0]}
```

### 动作流水

`POST /accounts/checkin` 与「领取全部福利」会往 `usage/activity.jsonl` 追加一行（时间 / 动作 / 账号 / 结果 / 积分 / 说明），
文件超过 5MB 自动轮转为 `.1`，读接口只返回最近 30 天。

