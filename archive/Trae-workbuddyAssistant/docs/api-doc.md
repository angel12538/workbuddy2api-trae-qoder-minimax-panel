# API 文档 — Tauri 前端 ↔ Rust 命令契约

> 前端通过 `invoke('command', args)` 调用。所有命令运行在 Rust 主线程（耗时操作放子线程并通过事件回传）。错误以 `string` 返回（空=成功），或以 `{ ok: boolean, error?: string }` 结构返回。

## 1. 环境检测

### `env_check()` → `EnvStatus`
- 返回：`{ installed: boolean, running: boolean, version: string|null, path: string|null }`
- 副作用：无。

### `open_trae_website()` → `void`
- 打开 `https://www.trae.cn`（未安装引导）。

## 2. CA 证书

### `cert_status()` → `{ installed: boolean }`
### `cert_install()` → `{ installed: boolean, error?: string }`
- 提权（UAC）将 `certs/ca.cer` 装入本地计算机受信任根证书颁发机构。

## 3. 代理服务

### `proxy_start(port: number)` → `{ ok: boolean, error?: string }`
- 启动 `device_proxy.py` 子进程（env: `PROXY_PORT`, `AUTO_CAPTURE_JWT=1`）。
- 事件 `proxy-log`（逐行）、`account-captured`（uid）。

### `proxy_stop()` → `{ ok: boolean }`
- 优雅退出（CTRL_BREAK）→ 超时强杀。

### `proxy_status()` → `{ running: boolean, port: number, captured: number, started_at: number|null }`

## 4. 账号与分组

### `accounts_list()` -> `Account[]`
- `Account`: `{ userId, name, groupId|null, jwt: string, jwtExpHours: number|null, jwtExpTimestamp: number|null, checkedToday: boolean|null, credits: number|null, deviceIdMasked: string|null, remainingCredits: number|null, cooldown: CooldownInfo|null }`
- `jwt` 为账号完整 JWT 原文（供前端「查看 JWT」弹窗展示与复制）；`jwtExpTimestamp` 为 JWT 过期 Unix 时间戳（秒），供前端格式化过期日期。
- `credits` 取 `credits_history.json` 中该账号**最新日期**的余额（非历史峰值），无记录时为 `null`。
- `remainingCredits`（v2.0 新增）：取 `remaining_credits.json` 中缓存的剩余积分，无记录时为 `null`。
- `cooldown`（v2.0 新增）：`{ errorType: string, cooldownUntil: number, reason: string }`，无冷却时为 `null`。

### `credits_history()` → `CreditRecord[]`
- 返回积分历史明细（按日期聚合给看板绘图）。
- `CreditRecord`: `{ date: string, user_id: string, credits: number, delta: number }`（`date` 为本地 `YYYY-MM-DD`；`credits`=当日余额，`delta`=当日新增）。
- 数据源 `credits_history.json` 由 `auto_checkin.py` 在签到时按日期**追加落盘**（自动裁剪到 90 天）。

### `account_add_manual(name, jwt, groupId?)` -> `{ ok, error? }`
### `account_update(userId, name?, jwt?)` -> `{ ok, error? }`
- 更新账号名称和/或 JWT。传 `jwt` 时重新解析 UserID 并同步写回（JWT 可能换了账号），同时刷新 `updated_at`；`name`/`jwt` 均可选，仅传需要修改的字段。
### `account_delete(userId, deleteProfile)` -> `{ ok, error? }`

### `groups_list()` → `Group[]`
- `Group`: `{ id, name, color, order, count }`

### `group_create(name, color)` → `{ id }`
### `group_update(id, name?, color?, order?)` → `{ ok }`
### `group_delete(id)` → `{ ok }`（账号回落未分组）
### `group_move(userId, groupId|null)` → `{ ok }`

## 5. 签到

### `checkin_start(opts)` → `{ ok, error? }`
- `opts`: `{ scope: "all"|"group:<id>"|"selected", user_ids?: string[], skip_checked_in: boolean, skip_expired: boolean }`
  - ⚠️ **字段名是 snake_case**（`user_ids` / `skip_checked_in` / `skip_expired`）。Tauri 命令的嵌套对象参数默认 snake_case，本文档早前写成 camelCase 是错的——按 camelCase 传会静默失效（`scope:"selected"` 配空账号列表）。
- 启动 `auto_checkin.py --json-stream --accounts ... [--retry N]`，通过事件 `checkin-progress` 回传 NDJSON 行（`start`/`account`/`done`）。
- 完成事件 `checkin-done`：`{ ok, already, failed, total }`。
- **异常退出兜底**：脚本若在发出 `done` 之前崩溃（导入失败 / 依赖缺失 / Python 异常），Rust 侧会在子进程退出后补发一个合成的 `done` 事件（`ok:0`、`failed:<已处理账号数>`、带 `error` 字段），避免前端永久停在「运行中」。

## 6. JWT 续期 / 切换 / 设备

### `jwt_parse(jwt)` -> `{ userId, expHours, expTimestamp, status: "ok"|"warn"|"expired"|"unknown" }`
- `expTimestamp` 为 JWT 过期 Unix 时间戳（秒）。
- `exp` 兼容整数与浮点数（依次尝试 `as_i64` / `as_f64` / 数字字符串解析）；`userId`（payload `data.id`）兼容字符串与整数。

### `switch_account(userId)` -> `{ ok, error? }`
- 调 PowerShell 切换器（非交互 `-Action Switch -UserId`）；事件 `switch-progress` 回传步骤。
### `device_reset(userId)` → `{ ok, error? }`
- 删除 `device_map.json` 该条目。

## 7. 日志 / 设置 / 邀请

### `logs_query(opts)` → `LogLine[]`
- `opts`: `{ log_type?: "proxy"|"checkin"|"switch"|"system"|"error", date?: "today", keyword?: string, limit?: number }`
  - ⚠️ 字段名是 `log_type`（不是 `type`）。
- `LogLine`: `{ time, type, message }`

### `settings_get()` / `settings_set(patch)` → `AppSettings`
### `invite_link()` → `{ url }`（固化邀请链接）

## 8. Python 子进程 NDJSON 协议（签到）

`auto_checkin.py --json-stream` 输出：

```
{"type":"start","total":6}
{"type":"account","index":1,"user_id":"4487…","name":"清风杜宾","status":"already","credits":8200,"delta":0}
{"type":"account","index":2,"user_id":"1556…","name":"青衣网络","status":"success","credits":8500,"delta":300}
{"type":"account","index":3,"user_id":"…","name":"…","status":"fail","code":1001,"message":"JWT 无效","error_type":"SessionDead","cooldown_until":null}
{"type":"account","index":4,"user_id":"…","name":"…","status":"fail","code":1005,"message":"额度不足","error_type":"PlanLimit","cooldown_until":1786700000}
{"type":"done","ok":5,"already":0,"failed":1,"total":6}
```

`status` 取值：`already`(已签) / `success` / `fail`。前端据此渲染进度与颜色。

- 账号行附带 `credits`（签到后最新余额）与 `delta`（本次新增积分，仅 `success` 有值）。前端积分看板据此**实时刷新**余额与「今日新增」，无需等待 `done`。
- 每次签到，`auto_checkin.py` 会把 `{date, user_id, credits, delta}` 追加写入 `credits_history.json`（已签/失败也写一行，delta 为 0），供 `credits_history()` 命令与看板趋势图读取。
- **v2.0 新增**：`fail` 行附带 `error_type`（`PlanLimit` / `SoftRate` / `SessionDead` / `NotFound` / `Server` / `Client` / `BusinessError` / `Unknown`）和 `cooldown_until`（Unix 时间戳，`null` 表示不冷却）。签到成功且积分 > 0 时自动清除该账号的冷却状态。
- ⚠️ 两个字段的实际形态与本节早前文档有出入：
  - `elapsed`（单账号耗时秒数）**当前未被任何 emit 发出**——TS 侧把它声明为可选字段，实际恒为 `undefined`。
  - `delta` 在无新增时以 JSON `null` 下发（不是省略），TS 侧应按 `number | null` 建模。

## 9. 数据 DTO 关系

- `UserID` 为所有关联主键；`Account.groupId` → `Group.id`；`Account.deviceIdMasked` 来自 `device_map.json`；`credits` 来自 `checkin_summary.json` / `credits_history.json`。
- `Account.remainingCredits` 来自 `remaining_credits.json`（v2.0 新增）。
- `Account.cooldown` 来自 `account_cooldowns.json`（v2.0 新增）。

---

## 10. API 服务（v2.0 新增）

### `api_server_start()` → `ApiServiceStatus`
- **无参数**。监听端口 / API 密钥 / 默认模型都从持久化设置读取（`app_settings.json` 的 `api_port` / `api_key` / `api_default_model`），因此**改这三项需重启服务才生效**。
- 端口默认 7864；`api_key` 为空时跳过 Bearer Token 鉴权。
- `default_model` 默认 `deepseek-v4-flash`。支持模型：`deepseek-v4-flash`、`deepseek-v4-pro`、`glm-5.2`、`glm-5.3`、`doubao-seed-2.1-pro`、`doubao-seed-2.1-turbo`、`minimax-m3`、`kimi-k2.7-code` 等（大小写不敏感）。
- 应用退出时自动停止服务释放端口。

### `api_server_stop()` → `{ ok }`
- 停止 API 服务。

### `api_server_status()` → `ApiServiceStatus`
- 返回 `{ running: boolean, port: number, total_requests: number, active_uid: string|null, last_error: string|null, started_at: number|null }`
  - ⚠️ **不含** `api_key` / `default_model` / `pool_size`（本节早前文档有误）。

### `pool_list()` → `ApiPoolFile`
- 返回 `{ enabled_uids: string[] }`（持久化到 `api_pool.json` 的入池账号集合）。

### `pool_set(uids: Vec<String>)` → `{ ok }`
- 设置账号池选中的账号（持久化到 `api_pool.json`）。参数名是 `uids`（不是 `userIds`）。

### `pool_status()` → `PoolStatus[]`
- 返回运行中池的逐账号实时状态（冷却 / 积分等）。**服务未运行时返回空数组**。
- ⚠️ 不是 `{ accounts, activeAccount, totalRequests }`（本节早前文档有误）。

---

## 11. 代理日志（v2.0 新增）

### `proxy_logs_list(opts)` → `ProxyLogListResult`
- `opts`: `{ keyword?: string, start_time?: number, end_time?: number, offset?: number, limit?: number }`
  - ⚠️ 字段名是 **snake_case**（`start_time` / `end_time`）。此前 Rust 侧结构体误标 `rename_all = "camelCase"`，导致这两个时间边界被 serde 静默忽略 → **时间筛选恒不生效**（界面显示已筛选、实际返回全部）。已修正。
- `ProxyLogListResult`: `{ entries: ProxyLogEntry[], total: number }`（**是对象不是数组**）
- `ProxyLogEntry`: `{ id, timestamp, method, host, path, status, size, sse_model?, sse_tokens? }`
  - ⚠️ **不含** `status_code` / `duration_ms` / `log_file` / `line_number`（本节早前文档有误）。

### `proxy_log_detail(id: String)` → `String`
- 返回单条代理日志的完整详情（**纯文本**，非结构化对象）。
- `id` 是 `"文件名:行号"` 组合串（**一个参数**，不是 `log_file` + `line_number` 两个）。

---

## 12. 剩余积分（v2.0 新增）

### `fetch_remaining_credits(userId)` → `number`
- 查询单个账号的剩余积分（从 TRAE 服务端实时获取）。成功返回**裸数值**（`f64`），失败返回错误串。
- 结果缓存到 `remaining_credits.json`。

### `refresh_remaining_credits()` → `number`
- 批量刷新所有账号的剩余积分，返回**成功刷新的账号数**（`usize`）。

---

## 13. 设备标识重置（v2.0 新增）

### `reset_device_ids()` → `{ ok, error? }`
- 执行 6 层设备标识重置。通过 NDJSON 事件流式返回进度，前端订阅 `device-reset-progress` / `device-reset-done`。
- ⚠️ 命令名是 `reset_device_ids`（**不是** `switch_reset_device_ids`）。
- ⚠️ **不会自动 UAC 提权**：第 5 层（注册表 `HKLM:\SOFTWARE\Microsoft\Cryptography\MachineGuid`）需要管理员权限，普通权限运行时该层被**静默跳过**（脚本输出「需要管理员权限，已跳过」）。因此普通权限下实际生效的是 5/6 层。

### `reset_device_code()` → `{ ok, error? }`
- 重置设备码（TraeReset 的 Rust 原生复刻）：轮换 machineid 与 storage.json telemetry 标识，并清除全部登录凭据键（强制登出 Trae 内所有账号），让服务端把本机识别为全新设备。**仅操作用户数据文件，无需管理员权限**。
- 事件：`device-code-reset-progress` / `device-code-reset-done`。

---

## 14. 每日积分快照（v2.1 新增）

### `credits_daily_list()` → `CreditsDailySnapshot[]`
- 返回每日积分快照列表（供积分看板三线趋势图使用）。
- `CreditsDailySnapshot`: `{ date: string, total: number, earned: number, consumed: number }`
  - `date`: 本地日期 `YYYY-MM-DD`
  - `total`: 当日所有账号剩余积分之和
  - `earned`: 当日获得积分（签到获得 + 非签到获得）
  - `consumed`: 当日消耗积分（`|total - earned - 昨日total|`）
- 数据源 `credits_daily.json`，每次刷新剩余积分时自动记录当天快照，保留 90 天。

---

## 15. 冷却状态管理（v2.1 新增）

### `cooldown_clear(userId)` → `Result<(), String>`
- 手动清除指定账号的冷却状态。
- 从 `account_cooldowns.json` 中移除该账号的冷却记录。

---

## 16. JWT 刷新（v2.1 新增）

### `refresh_jwt(userId)` → `Result<String, String>`
- 使用 `refresh_token` 调用 TRAE OAuth ExchangeToken API 刷新 JWT。
- 成功后原子写回新的 `accessToken` + `refresh_token` 到 `checkin_accounts.json`，返回新 JWT。
- 使用 `jwt_refresh_lock` 防止并发刷新，持锁后 double-check 文件防止重复刷新。
- 无 `refresh_token` 的账号返回错误（需手动重新捕获 JWT）。
