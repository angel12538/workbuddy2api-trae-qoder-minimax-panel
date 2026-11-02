# 配置参考

- [命令行参数](#命令行参数)
- [面板密码](#面板密码)
- [API Key 与出口绑定](#api-key-与出口绑定)
- [模型闸门](#模型闸门)
- [限额护栏](#限额护栏)
- [环境变量](#环境变量)

运行时配置都保存在 `accounts/settings.json` 里，大部分可以在看板「设置」页修改，重启后仍然生效。

## 命令行参数

```
python qoder_proxy.py [--host HOST] [--port PORT] [--lan] [--api-key KEY]
                      [--accounts-dir DIR] [--usage-dir DIR]
                      [--panel-password PWD] [--system-prompt TEXT] [--pid-file FILE]
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--host` | `$HOST` 或 `127.0.0.1` | 监听地址 |
| `--port` | `$PORT` 或 `8790` | 监听端口 |
| `--lan` | 关 | 监听 `0.0.0.0`，强制要求 API Key（未指定时自动生成） |
| `--api-key` | `$API_KEY` / `$QD_PROXY_KEY` | 启动级 API Key。看板里一旦配置了 Key，这个参数就不再生效 |
| `--accounts-dir` | `$ACCOUNTS_DIR` 或 `./accounts` | 账号与设置目录 |
| `--usage-dir` | `$QD_PROXY_USAGE_DIR` 或 `./usage` | 用量流水目录 |
| `--panel-password` | — | 启动时设置面板密码 |
| `--system-prompt` | 官方模板 | 请求中没有 system 消息时使用的系统提示词 |
| `--pid-file` | — | 把进程 PID 写入该文件，退出时删除（`stop-qoder-proxy.bat` 依赖它） |

## 面板密码

- 打开看板需要输入面板密码，默认是 `admin`。它和 API Key 互相独立
- 密码以 PBKDF2-SHA256 摘要保存，不存明文
- 登录会话有效期 7 天，网关重启后依然有效；修改密码会让所有已登录的浏览器下线
- 面板还在用默认密码时，看板不会显示明文 API Key。请先修改密码

## API Key 与出口绑定

在看板「设置」页管理，可以同时配置多个 Key：

- 每个 Key 有名称，可以单独启用 / 停用 / 删除，删除后立即失效
- 每个 Key 可以绑定出口：
  - **国际版**：走 `api3.qoder.sh`，连不上时依次切换到 `api2` / `api1`
  - **国内版**：走 `gateway.qoder.com.cn`
  - **不绑定**：跟随看板顶部的全局出口
- 请求的模型和 Key 绑定的出口不匹配时，直接返回 400，给出说明

**鉴权规则：**

- 只要有任何一个启用中的 Key（或启动时指定了 `--api-key`），`/v1/*` 就必须带 Key
- 否则不需要 Key，适用于本机单用户场景
- Key 可以通过以下任一方式传入：
  - `Authorization: Bearer <key>`
  - `x-api-key: <key>`（Anthropic 客户端使用）
  - 查询参数 `?key=<key>`

## 模型闸门

看板中没有这项设置的入口，需要直接编辑 `accounts/settings.json`，或者调用 `POST /settings/save`：

| 字段 | 作用 |
|---|---|
| `banned_models` | 全局禁止的模型 |
| `api_keys[].models` | 这个 Key 只能使用的模型，留空表示不限制 |

两个字段都支持 fnmatch 通配符（如 `qwen*`），不区分大小写。被拒绝的请求在本地直接返回，不会发到上游。三个入口（Chat / Responses / Anthropic）共用同一套判断。

## 限额护栏

看板「设置 → 事实性限额护栏」。账号触碰阈值后会暂停接单，请求自动切到其他账号，不用等上游返回 402 / 429。

| 项 | 单位 | 作用 |
|---|---|---|
| `reserve_credits` | 积分点数 | 账号余额 ≤ 该值时暂停 |
| `daily_token_limit` | token | 账号当日累计 token 达到该值时暂停 |
| `daily_credit_limit` | 积分点数 | 账号当日累计消耗积分达到该值时暂停（免费模型不计入） |
| `model_daily_token_limit` | token | 账号对某个模型的当日 token 达到该值时，只暂停这个模型 |
| `expiring_window_days` | 天 | 余额将在 N 天内到期的账号优先派发。这是派发偏好，不会拦截请求 |

- 每项都可以分别设置「全局」「国际版」「国内版」三层。区域一层留空表示继承全局，填 0 表示关闭
- 默认全部关闭
- Credits 是积分点数，实测账号余额在 0 到 1100 之间。不要按 token 的量级去填，否则永远不会触发
- 因余额不足被暂停的账号，会在 UTC+8 每天 10:00（官方重置时间）恢复；如果余额提前恢复，也会提前解除暂停

## 环境变量

### 前缀会话亲和（可选：内存 / Redis）

相同出站消息前缀优先复用已有的 Qoder 上游 `session_id`。默认关闭。
单进程可直接设置 `QD_SESSION_CACHE=memory`，不需要 Redis 或第三方 Python 包。
多进程共享或网关重启后需要复用映射时，可选 Redis 后端。

这里保存的是 **前缀摘要 → 上游 session UUID** 的路由索引，不是模型 KV Cache，
也不保存对话正文、上游访问 token 或 API Key 明文。实际推理缓存由 Qoder 上游控制；
稳定 session 不保证每次缓存命中，前缀变化、上游淘汰和路由变化仍会影响命中率。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `QD_SESSION_CACHE` | `off`（配置 Redis URL 时为 `redis`） | `off` / `memory` / `redis`；显式 `off` 优先 |
| `QD_SESSION_CACHE_TTL` | `3600` | 映射空闲有效期，秒，范围 60–86400；命中后续期 |
| `QD_SESSION_CACHE_MAX_ENTRIES` | `4096` | 内存后端最多保留的前缀映射条数（不是会话数），范围 1–100000，LRU 淘汰 |
| `QD_SESSION_REDIS_URL` | 空 | `redis://[:password@]host:6379/0` 或 `rediss://...`；通过环境注入凭证 |
| `QD_SESSION_REDIS_TIMEOUT` | `0.4` | Redis 每次连接/读写的超时，秒 |
| `QD_SESSION_REDIS_TTL` | `3600` | Redis 初版配置别名；`QD_SESSION_CACHE_TTL` 优先 |

按实际出站消息计算累计 HMAC，原子选择最长匹配前缀（内存锁 / Redis Lua）。前缀至少
包含第一条非空 user 消息，不会仅凭共同 system 合并会话。映射按 API Key 标识、上游
账号、区域、模型、工具和参数隔离；客户端提供 `X-Conversation-Id` 等会话 ID 时也隔离
不同会话。未提供 ID 时，同一 API Key 下完全相同的初始对话前缀可共享 session；
需要独立对话时应显式传会话 ID。账号故障切换后重新查找该账号自己的映射。

内存后端重启清空，过期/淘汰后重新生成 UUID；最多保留指定条数。
Redis 后端仅保存 HMAC 键与随机 UUID 值（共享部署需使用相同面板会话签名密钥），
可配持久化；只支持单机或代理入口，不跟随 Cluster 的 MOVED/ASK 重定向。
Redis 不可达、配置错误或缓存值非法时，回退到新的随机 session，可能冷启动但不阻断推理。
日志不打印连接地址或异常详情。普通推理用量记录包含 `session_affinity`（`memory-prefix` /
`redis-prefix` / `random`）和 `upstream_session_id`，用于区分路由复用与实际 token 命中。

可选 Redis Compose 源码构建方案：

    docker compose -f docker-compose.yml -f docker-compose.redis.yml up -d --build

Redis 仅开放 Compose 内部网络，数据由专用命名卷保存。
设置 `QD_SESSION_CACHE=off` 并重启网关即可恢复随机 session 行为。

### 基础

| 变量 | 默认值 | 说明 |
|---|---|---|
| `HOST` / `PORT` | `127.0.0.1` / `8790` | 监听地址和端口 |
| `API_KEY` / `QD_PROXY_KEY` | — | 启动级 API Key |
| `ACCOUNTS_DIR` | `./accounts` | 账号目录 |
| `QD_PROXY_USAGE_DIR` | `./usage` | 用量目录 |
| `QD_PROXY_DEFAULT_REALM` | `cn` | 首次启动时的全局出口（`cn` / `intl`）。之后在看板切换的结果会保存下来 |
| `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` | — | 访问上游时使用的代理，见[出口代理](deployment.md#出口代理) |
| `QD_MAX_PAYLOAD_BYTES` | `52428800` | 客户端请求体大小上限（50 MB） |
| `QD_MAX_BODY_MB` | `29` | 发往上游的请求体（编码后）上限，单位 MiB。超过时本地直接返回 413，不发请求、不影响账号。上游网关对单个请求体有约 30MB 的硬限制 |

### 路由与会话

| 变量 | 默认值 | 说明 |
|---|---|---|
| `QD_AUTO_REALM` | `1` | 请求没有指定区域时，按模型归属和账号可用性自动选区，见[双区路由](models.md#双区路由) |
| `QD_AFFINITY_BY_PREFIX` | `1` | 同一段对话固定使用同一个账号，以命中上游的前缀缓存 |
| `QD_AFFINITY_MAX_MSGS` | `0` | 对话超过 N 条消息后不再固定账号，`0` 表示不限制 |
| `QD_AFFINITY_DEBUG` | `0` | 打印会话亲和的调试日志 |
| `QD_INTL_GATEWAY` | `https://api3.qoder.sh` | 国际版推理主机。连接失败时依次切换到 `api2` / `api1` |

### 上游容错

| 变量 | 默认值 | 说明 |
|---|---|---|
| `QD_QUEUE_MAX_ROUNDS` | `6` | 上游排队（10605 / RESOURCE_LIMIT）时最多等待重试几轮 |
| `QD_QUEUE_WAIT_MAX_SECS` | `180` | 单轮最多等待的秒数 |
| `QD_QUEUE_WAIT_BUDGET_SECS` | `600` | 排队总共最多等待的秒数 |
| `QD_QUEUE_WAIT_JITTER` | `2` | 每次排队等待额外加上 0 到 N 秒的随机抖动，避免多个客户端同时唤醒 |
| `QD_SSE_HEARTBEAT` | `5` | 等待首字期间发送 SSE 心跳的间隔（秒），`0` 表示关闭 |

### 流量守卫

限制单个账号的并发与请求节奏，避免把账号打到并发过高、触发排队或风控。用途和边界见[团队与企业账号](accounts.md#团队与企业账号)。

| 变量 | 默认值 | 说明 |
|---|---|---|
| `QD_TRAFFIC_GUARD` | 未设置 | `on`：对所有账号启用守卫；`off`：全部关闭（包括团队账号）；未设置时由 `QD_TEAM_GUARD` 决定 |
| `QD_TEAM_GUARD` | `auto` | `auto` 只对识别为团队 / 企业的账号启用；`on` 对所有账号启用；`off` 关闭 |
| `QD_TEAM_MAX_CONCURRENCY` | `1` | 每个账号同时进行的请求数上限，超出的请求排队等待 |
| `QD_TEAM_MIN_GAP_MS` | `400` | 同一账号两次请求启动之间的最小间隔（毫秒）。需要等待时另加 0–120 毫秒随机抖动 |
| `QD_TEAM_WAIT_SECS` | `30` | 拿不到并发名额时最多等待的秒数，超时后放行并打印 WARN，不会卡死 |
| `QD_TEAM_MAX_CLIENTS` | `3` | 一小时内同一账号来自超过 N 个不同客户端 IP 时打印 WARN，提示可能被多人共用 |

### 请求改写

| 变量 | 默认值 | 说明 |
|---|---|---|
| `QD_STRUCTURED_TOOL_HISTORY` | `auto` | 工具调用历史以结构化形式发给上游。`auto` 仅在国内版的 Qwen / GLM / DeepSeek 上启用；也可设为 `on` / `off` |
| `QD_TOOL_RESULT_KEEP` | `2000` | 截短历史工具结果时，头尾各保留的字符数；`0` / `off` 表示不截短 |
| `QD_TOOL_RESULT_SCOPE` | `tail` | 截短范围：`tail` 只保留最后 N 条完整、`all` 全部截短、`turn` 是旧行为（会破坏前缀缓存） |
| `QD_TOOL_RESULT_KEEP_TAIL` | `1` | `tail` 模式下保留完整的最后几条 |
| `QD_TOOLCHOICE_NONE_DROPS_TOOLS` | `0` | 设为 `1` 时，`tool_choice="none"` 的请求会连同 `tools` 一起删掉（旧行为） |
| `QD_KEEP_TOPLEVEL_IMAGES` | `0` | 设为 `1` 时，图片除了放在消息内容里，还会在请求体顶层再放两份（旧行为，仅供排查。会让多图请求的体积变成 3 倍） |

### Anthropic 入口

| 变量 | 默认值 | 说明 |
|---|---|---|
| `QD_ANTHROPIC_MSG_SYSTEM` | `inline` | `messages` 里的 system 消息如何处理：`inline` 原位改为 user 消息（保持缓存前缀稳定）；`merge` 合并到顶层 system（旧行为）；`drop` 丢弃 |
| `QD_ANTHROPIC_THINKING` | `off` | 设为 `empty` 时输出带空 `signature` 的 thinking block（实验性） |

### 账号与设备身份

| 变量 | 默认值 | 说明 |
|---|---|---|
| `QD_CREDITS_USAGE_INTERVAL` | `300` | 后台刷新「最近用过的账号」余额的间隔（秒） |
| `QD_NATIVE_IDENTITY` | `1` | 签到 / 活动链路调用官方 UMID 组件获取真实机器身份 |
| `QD_UMID_DIR` | — | 提取的 UMID 组件所在的目录（默认 `<repo>/umid`） |
| `QD_RUNTIME_INFO_ENV` | 国内版 `0` / 国际版 `3` | 传给 `runtime-info` 组件的环境码，与官方桌面端一致（改变后会失效旧环境缓存） |
| `QD_MACHINE_IDENTITY_CACHE` | `auto` | 机器身份落盘缓存，设为 `off` 关闭 |
| `QD_MACHINE_IDENTITY_CACHE_TTL` | `0` | 缓存有效期（秒），到期后换新身份；`0` 表示永不过期 |
| `QD_MACHINE_IDENTITY_RESET` | — | 设为 `1` 时，启动后清空缓存并换新身份 |
| `QD_MACHINE_IDENTITY_VOTE` | `1` | 首次采集身份时多次采样取多数，设为 `0` 时只采一次 |
| `QD_DESKTOP_VERSION` | `0.4.8` | 覆盖签到请求里上报的桌面客户端版本号 |
| `QD_COSY_VERSION` | `1.1.75` | 覆盖推理 / 模型请求里上报的 CLI 版本号（`cosy-version` 请求头与签名 payload） |
| `QD_DEVICE_ID_LEGACY` | `0` | 设为 `1` 时，推理用的设备码保持 v1.5.9 之前的 32 位 hex 形态（默认是 UUIDv4 形态）。从旧版本升级、不想让上游把存量账号看成换了设备时使用 |
| `QD_COSY_OS_HEADERS` | `1` | 请求携带 `cosy-machineos`（SSE 另带 `cosy-machinehostname`）请求头，`0` 表示关闭 |
| `QD_COSY_MACHINE_OS` | 按运行平台 | 覆盖 `cosy-machineos` 的取值；默认 Windows 上为 `x86_64_win32`、其他平台为 `x86_64_linux`，设为 `auto` 时按系统与架构自动识别（`{x86_64,aarch64}_{win32,darwin,linux}`） |

### 启动脚本与调试

| 变量 | 说明 |
|---|---|
| `QD_PROXY_PYTHON` | `.sh` 启动脚本使用的 Python 解释器 |
| `QD_NO_PAUSE=1` | `.bat` 启动 / 停止脚本结束时不暂停 |
| `QD_TASKS_DEBUG=1` | 把任务中心每个子调用的耗时打印到 stderr |


## 本地 web 工具（`QD_WEB_TOOLS`）

客户端（Codex 等）在 Responses 请求里声明的内置工具 `web_search` / `web_fetch`，Qoder 上游不会执行；
开启本功能后由**网关本地执行**：把声明改写成同名 function，模型调用时代网关去搜索/抓页面，再把结果回灌给模型继续生成（最多 3 轮）。

| 开关 | 说明 |
|---|---|
| 看板「设置 → 本地 web 工具」 | 持久化在 `accounts/settings.json` 的 `web_tools`，改完立即生效 |
| `QD_WEB_TOOLS=on/off` | 环境变量优先于面板开关 |
| `QD_WEB_TOOLS_ROUNDS` | 代跑轮次上限，默认 3 |

安全边界：只允许 http/https；拒绝带凭据的 URL、localhost、私网与链路本地地址；
重定向逐跳复检；响应体 4MB / 正文 20000 字上限。
配置了 `HTTPS_PROXY` / `QD_WEB_TOOLS_PROXY` 时由代理做 DNS 解析（仍然拦字面私网地址）。

代价：每代跑一轮都会多消耗一次账号额度；默认关闭。

