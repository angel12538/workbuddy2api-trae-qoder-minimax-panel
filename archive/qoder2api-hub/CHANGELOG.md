# 更新日志

每个版本的发布说明同步在 [GitHub Releases](https://github.com/shuishuipingan/qoder2api-hub/releases)。

## 版本号规则

- 每次发布**照常递增补丁号**：1.6.0 → 1.6.1 → 1.6.2 …
- 补丁号累计到 **14** 之后，下一次发布升**次版本**并把补丁号归零：
  1.6.14 → **1.7.0**（不是 1.6.15，也不是 1.7.14）。
- 主版本号只在确有破坏性变更时才动。

## v1.7.10

**🐛 问题修复**

- **流式/非流式 usage 未归一为 OpenAI 标准位**（#112）：上游按模型族换缓存字段（DeepSeek `prompt_cache_hit_tokens`、Qwen `completion_tokens_details.cached_tokens`），出口原样透传，按标准协议解析的客户端读到的 `cached_tokens` 恒为 0。
  新增 `normalize_usage_for_client()`：复用 `_extract_usage` 的优先级把数字搬进 `usage.prompt_tokens_details.cached_tokens`，**原字段保留**（已适配上游形态的客户端不受影响）；接在流式 `clean_chunk`（usage 帧）与非流式 `aggregate_stream` 两处。上游完全没给缓存时不凭空造字段；普通内容帧仍逐字节原样透传。

**✨ 新增：计费模型自检**

- `POST /diag/model-billing {model, realm?}`（面板鉴权）：发一个最小请求（ping + `max_tokens=16`），返回 `{billed: [...], billed_mismatch, account, elapsed_ms}` —— 用来回答「上游到底按哪个模型记账」；
- 面板「模型库」每行新增 **自检** 按钮，结果直接 toast；
- `usage` 行的 `billed_model`（v1.7.8）保留，但**日志改为同 (请求, 回报) 组合每小时只提醒一次** —— 实测上游对网关身份常常统一回报 `auto`（连 `dfmodel` 也是），逐请求 WARN 会把日志刷满。

**🔧 客户端身份可覆盖（#113 排查用）**

- 新增 `QD_COSY_CLIENTTYPE`（默认 `5`，即 CLI 形态）与 `QD_COSY_UA`（默认 `undici`）环境变量，配合既有的 `QD_COSY_VERSION`：用户可以把**自己官方 CLI 的身份**原样搬过来做 A/B，验证上游是否按客户端身份决定模型授权/记账。
- 未伪造任何东西：`minimal_version.vsc = 999.999.999` 这类哨兵值只出现在注释里解释现象，不进代码分支。

**🌐 i18n（PR #114 合并）**

- 原生 `alert` / `confirm` / `prompt` 翻译层 `installDialogI18n`（只翻第一个字符串参数、其余透传、幂等）；`i18n/en.json` 扩到 608 消息 / 226 模式 / 26 片段；新增 `scripts/extract_i18n_keys.py` 提取工具与 `docs/i18n.md` 重写。
- ⚠️ PR 只定义了 `installDialogI18n` 却**从未调用、也没导出**（功能等于没做）。合并后补上接线：`initLangSystem` 启动时装一次（`installDialogI18n(window, t)`）+ 导出到 `window.QD_I18N.API`，并加 17 条回归断言（含「确实被装进语言系统」这条接线检查）。

**验证**：新增 `_test_issue112_usage.py`(15)、`_test_issue113_model_billing.py`(18)、`_test_i18n_dialog.js`(17)；`/diag/model-billing` 走真实服务 e2e（200 + 401 两条路径，实测返回 `billed=["auto"]`）。全量 **89 suites / 2462 checks / 0 failed**。

## v1.7.9

**✨ 新增**

- **账号管理表格支持按列排序**（#111）：版本 / 账号 / 状态 / 积分 / 请求 / 总 token / 缓存 七列可点表头，升序 → 降序 → 恢复默认（三态循环），当前列显示 ▲ / ▼，状态存 `localStorage` 刷新后保持。
  - **账号列自然序**：`Z Z2` 排在 `Z Z10` 之前（`localeCompare({numeric:true})`，忽略大小写与首尾空白，昵称缺失退回 UID）；
  - **积分列两种口径**：点击=剩余额度、`Shift+点击`=已用额度；
  - **取不到值的一律沉底且不随方向翻转** —— 额度拉取失败（`envelopes HTTP 403 code 112` 这类）不会被当成 0 混进「额度用尽」中间，降序时也不会翻到最上面；
  - 状态列按「可用 → 异常 → 冷却 → 停用 → 过期」排；护栏 / 操作列不参与排序；同值时按账号名自然序稳定排列。

**验证**：新增 `tests/_test_issue111_account_sort.js`（27 条：自然序、两种额度口径、失败沉底与方向无关、请求/token/缓存数值序、状态优先级、三态循环与持久化、表头锚点与指示符）；真实浏览器里点表头复核排序结果与 ▲▼ 指示。全量 **86 suites / 2413 checks / 0 failed**。

## v1.7.8

**✨ 新增**

- **模型上下文窗口可配置**（#106）：官方清单多数模型默认 200K 档，模型本身支持 1M/400K。
  - 面板「模型库」新增 **上下文设置** 列（下拉：默认 / 官方 context_config 各档位），写入 `settings.json.model_context` 立即生效；
  - 请求头 `X-QD-Context: 1M`（或 token 数）单次请求抬窗，chat / responses / anthropic 三端点都支持，优先级高于面板；
  - 生效位置是出站 `model_config.max_input_tokens`（与官方桌面端选档同一通道）；超出官方档位被忽略并 WARN；客户端无法用 body 里的 `_qd_context` 伪造（入口先清除，只信 HTTP 头）。

**🐛 问题修复 / 可观测性**

- **Kimi-K3 等被上游静默回退成 auto 计费，现在完全可见**（#105）：实测 CN 区 `kmodel_latest` 会被上游按 `auto` 路由计费（官方清单标 `minimal_version.vsc=999.999.999` = 该形态未放量，网关无法也不应伪造更高客户端版本）。
  - usage 行新增 `billed_model`（仅当上游 usage.model ≠ 请求模型时写入，其余行格式零变化）；
  - WARN 日志 `upstream billed a different model: requested=... billed=auto`；
  - 面板「最近请求」模型列出现红色 **计费→auto** 徽标——「以为在用 Kimi、实际在用 auto」一眼可辨。
- **额度类 403（code 112 额度耗尽 / 110 每日次数上限）快速失败**（#108，PR #109 合并）：不再当普通「账号被拒」——112 冷却 30 分钟、110 冷却到本地 0 点（封顶 6h），且**刻意绕开 single_account 的 3s 压缩**（单号池不再 3 秒一轮反复撞同一个耗尽账号）；不禁用账号、不喂降权，10605 排队路径不受影响；客户端侧 503 文案点明「请充值或启用其它账号」。

**🌐 面板 i18n（PR #107 合并）**

- 零依赖 DOM 边界翻译层：丢一个 `i18n/<code>.json` 进目录即出现在语言菜单（manifest 每请求重读，热加载）；未命中一律回中文（结构不变式 `t(t(x))===t(x)`，观察者必收敛）；
- 首个语言包 `i18n/en.json`（343 消息 / 68 模式 / 10 片段）；`/i18n/*` 只读白名单 + realpath 收敛（挡住 `..` 与指向 accounts/ 的软链）；语言选择在本地记忆；
- ⚠️ 该 PR 同时把 14 个无关文件整体换成 CRLF 行尾（workflows 的多行 `run:` 在 ubuntu runner 上是经典炸点）。已在合并后单独还原这些文件为仓库既有 LF blob（**内容零差异**），净 diff 只含 9 个实质文件、2355 行纯新增。

**验证**：新增 `_test_issue105_106.py`(23)、PR 侧 `_test_quota_envelope.py`(21) + `_test_i18n_*.js/py`(104)；设置接口走真实服务 e2e（401/存/取/清全链路）；全量 **85 suites / 2386 checks / 0 failed**。

## v1.7.7

**🐛 问题修复**

- **DSH「一键配置」写入未加引号的 YAML 值，纯数字 Key 让 DSH 完全起不来**（#103 / PR #104）：`yaml_refs_upsert()` 用字符串插值直接拼 `key: value`，YAML 会把 `13800138000`、`007`、`1e5`、`true`、`null` 解析成非字符串；DSH 的 credentials-local 对 `refs` 值做硬校验（`typeof !== "string"` 直接抛 `TypeError`），于是 `credentials` 插件不激活 → `connection`/`authorization` 与全部 tauri、mcp 插件级联 waiting → Harness 进程 exit 1，而面板全程显示「配置成功」。
  现在统一用 `json.dumps()` 生成**双引号标量**（同时也是合法 YAML double-quoted scalar，转义覆盖换行/引号/反斜杠/控制字符）；三处写入点全部走它。
  另外补了**写后自检**：`refs_value_is_scalar_string()` 校验写入结果，不合格就让 `plan()` 直接抛错——面板不会再报「配置成功」。

**✨ 新功能**

- **禁用与签到解耦**（#102）：`enabled` 只表示「不接推理流量」，不再顺带停掉每日签到。
  - 账号新增 `checkinWhenDisabled`（默认关 = 维持历史行为）；停用的账号行多一个 **仍签到** 勾选；
  - 口径拆成两个谓词：`inference_eligible()`（`enabled + token`，推理选号不变）与 `checkin_eligible()`（有 token 且「启用或开了开关」）；
  - 面板每日签到、任务中心批量签到/领福利、调度器整点签到三条路径都改用后者；**凭证失效/过期仍排除**（`stale_uids` 逻辑不动）；
  - 新接口 `POST /accounts/checkin-when-disabled {uid, value}`，落盘即生效。

  副作用是好的：照常签到让认证会话保持活性，重新启用时更不容易撞上「token 早已静默过期」。

**验证**：`_test_issue103_yaml_refs_quoting.py`（PR 自带 74 条）、新增 `_test_issue103_selfcheck.py`（17 条：十种危险 Key 都能安全写入 + 自检函数四态）、新增 `_test_issue102_checkin_decoupled.py`（18 条：两谓词语义、持久化/public、三条路径源码守卫、端到端落盘回读）；全量 **80 suites / 2236 checks / 0 failed**。

## v1.7.6

**🐛 问题修复**

- **排队耗尽后 `RateLimited.status` AttributeError，200 的 SSE 被硬截断**（#100）：`visible_queue_acquire` 放弃时会把排队异常归一化成 `RateLimited`（只有 `http_error/detail/wait`，**没有 `.status`**），而 `/v1/responses` 与 chat 的 pump 收尾路径直接读 `pump_exc.status` —— 于是 200 已经发出去、流里一个字节都没补，客户端只看到连接被截断。
  新增 `_exc_http_status(exc, default)` 统一取状态：`UpstreamStatus/Queued -> .status`、`RateLimited -> 429`、`HTTPError -> .code`，取不到才回落默认值；responses 与 chat 两处收尾都改为走它，并且仍按协议发终态事件（`response.failed` / `data: {error}` + `[DONE]`）。

- **`max_tokens` 超上游上限直接透 400**（#101）：官方清单没有 `maxOutputTokens`，客户端只能踩坑试出来。现在把上游那句 `Range of max_tokens should be [1, N]` 当**学习信号**：
  - `learn_max_output_limit()` 解析 `N`，按 `(区域, 模型)`（同时记客户端名与解析后的上游 key）落盘到 `usage/max-output-limits.json`，7 天 TTL；
  - `build_qoder_body()` 在组装请求时就 clamp（含 `/v1/responses` 的 `max_output_tokens`），并打 WARN；
  - `/v1/models` 补上 `max_output_tokens` / `max_completion_tokens`，客户端可自行预判；
  - 记录点接在 `record_error`，任何入口命中该错误都会学到。

  语义：第一次超限仍会收到上游 400（网关靠它才知道 N），**之后同一模型自动压到上限**。

**验证**：新增 `tests/_test_issue100_101.py`（22 条：状态提取四类异常、两处收尾的源码守卫、上限解析/落盘/重载/clamp 生效与不生效、请求体实测 clamp、`/v1/models` 补字段）；全量 **77 suites / 2127 checks / 0 failed**。

## v1.7.5

**🔄 跟进官方桌面端新版（CN 0.4.3 / intl 0.4.8）**

逆向了两版客户端的 bundle，把「我们已经落后」和「可以顺手增强」的部分分开处理。

**✨ 新增：官方积分消耗（真实记账口径）**

- 客户端新增 `/sash/api/v1/ai-conversations/credits-summary`（累计消耗 + 单日峰值 + 峰值日）与 `credits-heatmap`（近 365 天逐日消耗），cn / intl 实测均 200；
- 网关新增 `Account.fetch_credit_usage()`（按账号缓存 5 分钟）与 `GET /accounts/credits/usage`（汇总 + 逐日合并，支持 `?uid=` / `?realm=`）；
- 面板「用量洞察」新增 **官方积分消耗** 区块：累计 / 峰值摘要 + 365 天热力图 + 按账号明细表（含查询失败标记）；前端另有 60 秒节流与签名去重，不跟着 5 秒轮询打。

**🩹 增强：国际版活动平台第二入口**

- `center.qoder.sh` 实测对 intl 的 `/sash/...` 返回 200、对 cn 返回 401，已作为 intl 的候选基址排在官网域名之前（`openapi_candidates` 仍只在连接失败/404 时才换）。

**📌 已封成能力、暂未上界面**

- `/sash/api/v1/me/achievements`（已获成就 key）→ `Account.fetch_achievements()`，用于解释「活动需先完成某成就」，面板展示留到下一版。

**🔍 查过但不采用的**

- `/api/v2/service/region/endpoints`、`/api/v2/me/usage`：用桌面端 token 在 openapi 上是 404（`api.qoder.com` 上是 401，属另一套鉴权），暂无可解析契约——intl 推理主机仍用硬编码 api3/api2/api1 + `QD_INTL_GATEWAY` 覆盖；
- 客户端版本号：本机 CN 实际是 **0.4.3**（随包带了 `resources/umid/runtime-info.exe`），我们的 `DESKTOP_CLIENT_VERSION=0.4.8` 来自 intl，仍可用 `QD_DESKTOP_VERSION` 覆盖。

**🧪 测试**：新增 `_test_credit_usage.py`（21 条：端点常量 / 解析 / 缓存命中 / 失败回退 / 路由 / 面板 / 节流 / 去重）；修掉 `_test_qoder.py` 里两处依赖「本机没有 runtime-info 组件」的断言（新版客户端已随包提供，改为显式钉 derived 身份再验证）。全量 **76 suites / 2105 checks / 0 failed**。

## v1.7.4

**🐛 问题修复**

- **Anthropic 入口（`/v1/messages`）漏接排队分支**（#99）：`open_upstream()` 在 `visible` 模式下把 10605 排队抛给上层由各入口等待，但 `_handle_anthropic` 的 except 链里只有父类 `UpstreamStatus`，没有 `UpstreamQueued`——于是排队异常掉进兜底分支：**零等待**直接 503，并且**丢掉 `Retry-After`**。聚合网关（NewAPI 的 Claude 渠道）因此无法按上游建议退避，只能立刻重试。

  现在与 `/v1/chat/completions`、`/v1/responses` 对齐：
  - `visible` + 流式：建流后**流内等待**，用 anthropic 的 `ping` 事件携带 `qoder_queue` 报进度，超预算给 `event: error`（`overloaded_error`）收尾；
  - `fast` 或非流式：直接 503 + `Retry-After` + 中文原因；
  - 兜底的 `except UpstreamStatus` 也会透传 `retry_after`，避免将来出现新的排队形态时再次丢掉退避信息。

  为什么这里可以安全地做流内等待：排队发生在 `open_upstream()` 阶段，此时**一个字节都还没写给客户端**（`_sse_begin()` 尚未调用），不存在 docstring 里担心的「重复 `message_start`」问题——那条能力差异只适用于 `aggregate_stream` 之后的分支。

**验证**：新增 `tests/_test_issue99_anthropic_queue.py`（18 条：源码守卫 + 用桩 Handler 实测三条路径——非流式 503+Retry-After、`fast` 模式不建流、`visible` 流式发 ping 排队帧并在超预算时发 `event: error` 且不返回 `_error`）；全量 **75 suites / 2084 checks / 0 failed**。

## v1.7.3

**🐛 问题修复**

- **数据看板轮询时动画反复播放、图表跳动**（#98）：`loadAnalytics()` 每 5 秒重新拉数据并**整块重绘**，于是数字滚动、排行进度条增长、柱状图 `height` 过渡、折线描边、热力图 365 格重建、洞察四卡 + 三张表全部重播一次——数据没变也照播。
  现在两层防护：
  - **分区签名**（`statSig`）：KPI、四张排行、热力图、缓存四卡、24h 曲线、按天趋势、按 Key 档位表各自记忆上次渲染的数据签名，**没变就整段跳过**；热力图用它避免每 5 秒重建 365 个节点。
  - **动画只在首屏 / 换区间 / 手动刷新播放**（`statAnimateOnce`）：轮询走「静默更新」，数字直接落值、进度条直接落宽度、柱状图直接 `grown`、折线不描边。
  - 文本写入也去重（`statSetText`）：相同字符串不再写 DOM（赋值本身也会触发 characterData 变更）。
  - 「用量洞察」同样加了 `INSIGHT_SIG` 早退与 `INSIGHT_ANIMATE` 开关。

  实测：看板静止 12 秒（跨 2+ 次轮询）期间，`statKpis / heatWrap / 四张排行 / hourlyChart / dailyChart / insightCards / cacheCards / effortByKey / statQuality` 的 **DOM 变更次数从每次轮询数百次降到 0**。

**验证**：新增 `tests/_test_issue98_flicker.py`（26 条：八处签名早退、文本去重、动画开关、以及 `statSig` 的语义自测——首次 true / 相同 false / 变化 true / key 隔离 / 按内容比较）；全量 **74 suites / 2066 checks / 0 failed**。

## v1.7.2

**🐛 问题修复（来自社区反馈）**

- **导入本地凭证报「导入失败」但账号其实导入成功**（#95 / PR #96）：v1.7.1 重做看板时删掉了性能矩阵容器 `#perfMatrix`，但 `refreshInner()` 里操作它的约 138 行死代码没跟着删——每 5 秒轮询抛一次 `TypeError`；`doImportDesktop()` 又把它和导入请求放在同一个 try 里，于是被报成「导入失败」。现在删掉死代码，并把「导入」与「导入后刷新界面」拆成两段兜错。
- **面板 `.hint` 类名冲突，说明文字被压成 14px 竖条**（#92 / PR #94）：同一个 `.hint` 既用作 12px 说明文字、又被写成 14×14 的「?」图标，后者覆盖前者，34 处受影响（28 处文字全中招）。图标类改名 `.hint-icon`，文字恢复 12px 正常排版；顺带修掉侧栏标签提取里 `\s` 写成 `s` 的老 bug。
- **Windows 启动脚本三项缺陷**（#91 / PR #93）：
  - 陈旧 PID 被无关进程复用 → 脚本误判「已在运行」而**从不拉起服务**：改为 `Get-CimInstance` + 校验命令行含 `qoder_proxy.py`（与 stop 脚本同标准）；
  - 跑在**别的端口**时也被标成「已在运行」并给出无人监听的 Dashboard：新增独立分支，明确拒绝并解释（单 pid 文件不能并存两个实例）；
  - 代理变量大小写重名（`HTTP_PROXY` 与 `http_proxy` 同时存在）让 PowerShell 5.1 的 `Start-Process` 静默失败、脚本仍打印 `[OK] started`：先清理小写别名，再用**真实 TCP 连接**验证端口起来，失败一律非零退出；
  - `qoder-proxy.pid` 不再被 git 跟踪（陈旧 PID 会随仓库分发）。

**✨ 新功能（可选）**

- **前缀 session 亲和（PR #97，默认关闭）**：`QD_SESSION_CACHE=memory`（单进程，标准库，TTL + LRU）或 `=redis`（多进程共享，`QD_SESSION_REDIS_URL`，Lua 原子选最长前缀）。键是「隔离范围 + 累计消息前缀」的 HMAC，值是随机上游 session UUID——不含对话正文、API Key 明文或模型 KV Cache。按 Key 标识 / 实际账号 / 区域 / 模型 / 工具参数 / 显式会话 ID 隔离。Redis 不可达或配置非法时回退随机 session，不阻断推理。附带 `docker-compose.redis.yml` 与配置文档。默认关闭，行为与之前一致。

**验证**：合并四个 PR 后全量 **73 suites / 2040 checks / 0 failed**（新增 `_test_prefix_session_cache.py` 41 条含并发与回退用例）。

## v1.7.1

**✨ 数据指标看板重做（统计概览）**

- 顶部「区间胶囊」：今天 / 7 天 / 30 天 / 本月 / 全部 / 自定义（自定义时显示两个日期选择器，带滑块指示器）；
- 六张 KPI 卡：总请求数、成功请求数、总 Token、活跃天数、当前连续天数、Top 模型；
- 四张排行面板：Top 账号 / Top 提供商 / 模型用量 / 提供商用量（进度条 + 悬浮明细）；
- **活跃热力图**：近 365 天按天 Token 着色，5 档图例按本机峰值自适应，月份只在变化处标注；
- **缓存命中率**：近 10 分钟 / 1 小时 / 24 小时 / 7 天四卡（命中 / 输入）；
- **近 24 小时双轴趋势**：左轴命中率、右轴总 Token，无请求整点按 0% 计但不写读数；
- **按天 Token 趋势**：柱状图，空区间给明确占位；
- 新增后端聚合接口 `GET /usage/stats`（单次扫描 `usage.jsonl` 产出上述全部数据；厂商由模型名推断，账号 UID 自动映射昵称）。

**🧩 页面结构整理**

- **账号独立成页**：账号列表、搜索、区域卡、账号 KPI、签到与福利中心、最近请求全部迁到「账号」页；网关页只保留「网关出口与调度」「运行概览」「模型库」三块；
- **用量洞察并入数据看板**：导航由 7 项收敛到 6 项，洞察区块作为看板页最后一个分区（标注为全量口径，不随区间变化），随看板一起 30 秒刷新；
- 账号页新增**搜索**（昵称 / UID / 组织名 / 备注，160ms 防抖，无结果有独立空状态）、吸顶工具栏、行入场动画。

**🎨 产品化视觉层**

- 建立设计令牌（圆角 / 阴影 / 缓动 / 时长 / 字体栈 / 面板渐变）；
- 卡片统一「描边 + 分层投影 + 极轻渐变」，只有可交互卡片才 hover 抬升；
- 表格改为小字距大写表头、行分隔线、数字等宽、主题色 4.5% 行 hover；
- 按钮与表单建立基线（30px 高、8px 圆角、明确 hover/active/focus 态）；
- 侧栏改为卡片式容器、active 用 2px 主题色左条；细滚动条、`:focus-visible` 光环、深浅色分别调校。

**🐛 问题修复**

- **`range` 形参遮蔽内建 `range()`**：新接口首版 500、连接被直接断开（`TypeError: 'str' object is not callable`）；
- **面板标题 `<header>` 继承全局吸顶**：滚动时面板标题盖住顶部导航（`header{position:sticky;z-index:10}` 收窄为 `body>header`，并给面板标题显式清吸顶）；
- **首屏任一 await 抛错就不再切页签**：表现为「页面出来了但数据永远空白」，boot 每步独立容错；
- **预点亮 TABS 缺 `accounts`/`insights`**：刷新会回落到网关页；
- **`RECENT_SNAPSHOT` 被误删**：网关页报「账号加载失败」；
- **面板标题用 `h3` 导致左侧导航整块消失**（`pageNavSections` 只认 `h2`）；顺手让侧栏标签不再带「?」；
- **侧栏折叠是全局记忆**：任一面折叠后其它页只剩 44px 细条，改为按页面记忆、默认展开。

**验证**：新增 `_test_stats_overview.py`(28)、`_test_accounts_page.py`(22)；更新 `_test_insights_page.py`(25) 与两套旧看板 JS 套件锚点；全量 **72 suites / 1997 checks / 0 failed**。

## v1.7.0

**✨ 企业 / 团队账号（#90）**

企业账号一直能用（额度走组织共享池），但**判定口径是错的**：此前只看 `userType` 含 team/enterprise，或额度里有共享池；而 `/api/v1/userinfo` 根本不返回 `user_type`，于是企业号被当成个人号——团队闸门不生效、共享额度不计入有效余额。

- **判定统一到 `Account.is_team()`，三层优先**：① 组织字段（`organization_id` / `organization_name`，OAuth 登录即有）→ ② `userType` → ③ 额度快照里的共享池（含 `cap=-1` 无限池）；proxy 侧的 `_is_team_account()` 改为委托它，两处口径不再分裂。
- **面板徽标**：企业/团队账号在账号名旁显示紫色的 **企业 · 组织名**（`public()` 新增 `isTeam` / `orgLabel`），一眼分清个人号与组织席位。
- **登录字段兼容**：`organization_id` / `organization_name` 同时接受 camelCase 形态，避免换了个上游版本就丢组织信息。
- **文档**：`docs/accounts.md` 新增《怎么把企业账号接进来》与《「企业号抓不到」的常见原因》——含 SSO 强制场景改用「导入 PAT」、`cap=-1` 无限池的显示口径、以及组织字段为空的含义。

**验证**：新增 `tests/_test_issue90_enterprise.py`（21 条：三层判定、orgLabel 回退、public 字段、面板徽标、camelCase 兼容、文档）；全量 **70 suites / 1941 checks / 0 failed**。

## v1.6.14

**🐛 问题修复（macOS / Linux 常驻场景，#89）**

- **防双开把「正在退出的旧进程」当成健康实例**：启动探测只问「有没有人应答 `/health`」，于是 `kill 旧进程 && 立刻 start` 这种最常见的升级姿势里，旧实例还在收尾、仍能应答，新实例就打印「已有一个反代在运行」并**主动退出**——服务一个都没起来，日志却在说「别重复启动」。
  现在：`/health` 返回 `pid` / `started_at` / `started_iso`；启动时按**应答者的 pid 是否还活着**分类：
  - `stale`（pid 已退出）→ 提示「旧实例正在释放端口」并等待重试（最多 3 轮），继续启动；
  - `healthy` → 仍按双开处理，但提示里带上 **PID / 版本 / 启动时间**，并指向官方停止脚本；
  - `foreign`（别的服务占端口）→ 保持原有的明确报错。
  另外 `bind` 失败也加了 3 次重试（每次 1.5s），并在重试期间复查 `/health`——如果确实是健康实例起来了，按「已在运行」退出而不是报错。

- **macOS / Linux 没有官方停止手段**：`start-qoder-proxy.sh` 从不传 `--pid-file`，`qoder-proxy.pid` 永远是陈旧值，常驻（nohup / launchd / tmux）时只能靠 `lsof` 猜 PID。
  现在：`start-qoder-proxy.sh` 写 `--pid-file`，并新增 **`stop-qoder-proxy.sh`**（对齐 `.bat` 的严谨程度）：
  - 读 `qoder-proxy.pid` → 校验命令行含 `qoder_proxy.py` → 只停那一个 PID；
  - 先 `SIGTERM`，10 秒不退再 `SIGKILL`；
  - pid 文件缺失时按「进程名 + `--port`」兜底，且**拒绝**停不是 qoder-proxy 的进程；
  - 结束时清理 pid 文件，过期 pid 文件会被识别并移除。

**📚 文档**：README 的平台表、`docs/deployment.md` 的脚本表与升级步骤都补上 macOS / Linux 的停止方式与常驻说明。

**验证**：新增 `tests/_test_issue89_startup_stop.py`（20 条：实例分类四种情况、启动/绑定重试、提示文案、脚本语法与安全校验，POSIX 上还会真跑一次「启动→读 pid→停止→进程消失」）；全量 **69 suites / 1920 checks / 0 failed**。

## v1.6.13

**✨ 新增功能**

- **排队可见 / 快速失败（`QD_QUEUE_WAIT_MODE`）**：`visible`（默认）流式请求先建流并持续发心跳 + `qoder_queue` 提示帧；`fast` 立即返回 503 + `Retry-After`；`wait` 保留旧行为。
- **排队中文原因**：`上游模型排队中（10605 / RESOURCE_LIMIT）：请稍后重试，或换一个模型/区域。这不是你的 Key、账号或网络问题。`
- **面板质量指标**：`/health.quality` + 分析页卡片——首字延迟 p50/p90、输出 tok/s、排队率、回读命中。

**🐛 问题修复**

- **Responses（Codex）路径接入回读链**：三条入口（chat / Anthropic / Responses）行为一致。
- **`/v1/responses` 兼容补齐**：`stream_options` 透传、`parallel_tool_calls` 默认 True；未实现字段显式告警一次。
- **测试隔离**：runner 统一剥离 `HTTP(S)_PROXY` / `NO_PROXY`（开发机代理会让本机套件被回 502），并修掉 #70 排程套件的时间依赖 flake。

**验证**：68 suites / 1899 checks / 0 failed。

## v1.6.12

**🐛 问题修复**

- **账号测试可能测到别的账号**（#86）：`/accounts/test` 调 `open_upstream()` 时没钉账号，池子会按会话粘性/轮询挑一个——点了 A 号实际测的是 B 号，结果自然对不上。
  现在 `open_upstream()` 新增 `only_account=` 参数：
  - 账号测试固定使用被点的那个号（不参与轮换、不走粘性）；
  - 区域跟随该账号（避免自动路由把请求换到别的区）；
  - 按「单账号池」处理（冷却压到 3s，不会因为测试失败把整个池子的轮换节奏带偏）；
  - 成功时清理的是**实际**被测账号的错误状态（`acc.clear_error()`），而不是最初选中的那个。
  不传该参数时行为与之前逐字节一致（普通请求仍走池）。

**验证**：新增 `tests/_test_issue86_account_test.py`（8 条：源码守卫 + 钉死账号不会被池替换 + 未钉死仍走池）；全量 **67 suites / 1878 checks / 0 failed**。

## v1.6.11

**✨ 新增：账号行显示「今日是否已签到」与「积分变化」**

- **今日签到徽标**：账号名旁直接显示 **✓ 今日已签到**（绿）或 **⏱ 今日未签到**（黄），悬停可看具体日期（`Account.public()` 新增 `checkedInToday` / `lastCheckinDate`）。
- **积分变化 chip**：积分列在余额后显示最近一次变化——签到 `+100`（绿）/ 消耗 `-35`（红），带弹出动画与时间悬停提示。
  - 只在数值**真的变化**时更新（`fetch_credits` 对比上一次 `remain_effective`），余额不变不会把旧的 `+100` 冲掉；
  - 变化值与时间随账号落盘（`lastCreditsDelta` / `lastCreditsDeltaAt`），重启后仍能看到最近一次变化。

**验证**：新增 `tests/_test_account_checkin_status.py`（19 条：今日判定、public 字段、+100/-100/不变三种变化、落盘回读、面板渲染与样式）；全量 **66 suites / 1870 checks / 0 failed**。

## v1.6.10

**🐛 问题修复**

- **启动 bat 无法处理带空格的路径**（#85）：`Start-Process -ArgumentList '%SCRIPT%','--port',...` 的每个元素**不会自动加引号**，路径里有空格时会被拆成多个参数（`C:\Users\My` / `Project\qoder_proxy.py` …），python 直接报找不到文件。
  现在：
  - 用 `[char]34` 拼出双引号（避开 bat / PowerShell 双重转义），脚本与 pid 路径都作为**单个带引号参数**传入；
  - 路径经环境变量（`$env:QD_SCRIPT` 等）传入，避开 `&`、`^` 这类 cmd 元字符；
  - 日志重定向同样用变量，空格路径不再出错；
  - `-WindowStyle Hidden` / `-PassThru` / `--pid-file` 语义不变（#50 行为不变）。

  实测：在 `...\my project\qoder proxy\` 这种双层空格目录下启动，桩脚本收到的 argv 为 `['…\qoder proxy\qoder_proxy.py', '--port', '18766', '--pid-file', '…\qoder-proxy.pid']`，参数完整未拆分。

**验证**：新增 `tests/_test_issue85_bat_spaces.py`（9 条：源码守卫 + ASCII-only + 真实带空格目录端到端）；全量 **65 suites / 1851 checks / 0 failed**。

## v1.6.9

**🐛 问题修复（国际版签到不可用 + 无提示）**

- **原生机器身份缺失时 `desktop_headers()` 直接崩**：`native_machine_identity()` 在容器/桥失败时返回 `None`，`ident.get("machineToken")` 抛 `AttributeError`，任务异常而面板**不显示任何原因**。已按空 dict 处理，走 derived 分支。
- **无原生身份时活动列表可能被隐藏**：新增「最小机器头」兜底——当 `showCampaign=false` 且机器头为 omitted 时，只用 `cosy-machineos` + `cosy-machinehostname` 重试一次（实测该组合仍被服务端视为官方客户端；六头全发才会被过滤）。可用 `QD_INTL_MACHINE_FALLBACK=off` 关闭。
- **失败原因可见**：签到行在 `showCampaign=false` 时直接写明「缺原生机器身份 / 账号不在定向 / 出口被风控」，并给出安装命令 `python _install_umid.py --platform linux`。

**⚡ 签到速度（实测 12.5s → 1.05s/账号，3 账号整批 3.5s）**

- **legacy 端点不再逐个基址等超时**：`checkin_status`（国际版必然 404）此前 404 后仍换基址重试，单账号耗时 **12.5s**；现在只探主基址（`fallback=False`）、`retries 2 → 1`，并把 404 结果缓存 **6 小时** → 首次 1.05s，之后 0.00s。
- **签到后刷新并行**：`fetch_credits` + `fetch_plan` 从串行改为并行（省约 1.2s）；去掉无意义的整秒等待（≤0.3s）。
- 批量并发 3（`QD_CHECKIN_CONCURRENCY`），首个任务立即开始、其余 0–0.3s 错开。

**✨ 新增：用量洞察独立板块**

- 顶部导航新增 **用量洞察**（`#pageInsights`），从网关页拆出；四张**图标卡片**（等价花费 / 积分包 / 最快耗尽账号 / 签到成功率）+ 三张明细表；
- 数字**滚动动画**、进度条过渡、卡片悬停抬升与图标缩放、表格行 stagger 入场、刷新按钮旋转态，全部尊重 `prefers-reduced-motion`；
- 只在切到该标签时加载并 30s 轮询（不干扰其他页面）。

**🎨 全局交互打磨**：导航/按钮位移与按压缩放、卡片悬停、表格行过渡、`:focus-visible` 焦点环、`::selection` 配色。

**验证**：新增 `_test_insights_page.py`（19 条）、`_test_checkin_fallback_speed.py`（18 条）、`_test_intl_checkin.py`（13 条）；全量 **64 suites / 1842 checks / 0 failed**。真实 intl 账号实测领取成功（+100 Credits），批量 3 账号 3.54s。

## v1.6.8

**🔍 可诊断性（#84：聚合网关侧报 `do_request_failed`）**

这次没有改业务逻辑——排查结论是**请求根本没到达网关**（附件日志里没有任何 `/v1/chat/completions` 或 `chat:` 行），而 `do_request_failed` 是聚合网关的**传输层**报错（DNS / 连不上 / TLS / 超时）。但这类故障在两边都不好自证，所以补了三处：

- **未知路径返回带提示的 404**：重复的 `/v1` 前缀（`/v1/v1/chat/completions`，base URL 已带 `/v1` 又拼一次）会明确写出「把渠道地址填成 `http://<host>:8790`（不带 /v1）」；普通 404 仍原样回显路径。
- **未知路径记 WARN**：`404 unknown path: ...` 进日志，一眼分辨「请求到了但路径错」vs「请求根本没到」。
- **文档补《接入 new-api / one-api 等聚合网关》**：三类常见失败（地址不可达 / 重复 `/v1` / Key 不一致）的对照表，以及在**聚合网关容器内**执行的三条自检命令。

**验证**：新增 `tests/_test_issue84_unknown_path.py`（7 条）；本地起真实网关验证 `/v1/v1/chat/completions` 返回 404 + 提示、普通 404 不受影响；全量 **61 suites / 1792 checks / 0 failed**。

## v1.6.7

**🐛 问题修复（严重）**

- **跨区 failover 死循环，请求永不结束、日志刷屏**（#83）：`_alt_realm_ready()` 用 `"intl" if realm == "cn" else "cn"` 取「另一个区」——而 #76 之后 `realm` 可能是 `None`（未指定出口），`None` 走 else 分支得到 **cn**。于是当请求实际已在 cn 上排队时，网关把「另一区」也算成 cn：**切到 cn → 又排队 → 再切 cn**，无限循环，每个请求都拖到超时，客户端只看到 `upstream stream ended before a completion event`。
  修法两处：
  1. `_alt_realm_ready()` 先把 `realm` 解析成**实际出口**（`realm or detect_model_realm(model) or CURRENT_REALM`）再取另一区，且**解析结果与目标相同就返回空**；
  2. 两条流式路径的跨区 failover 加「每请求只切一次」标记（`_switched71`），切过一次后仍排队就直接转 **429 + Retry-After**，不再反复切。

  修复后的行为：cn 排队 → 若 intl 同模型有就绪账号则切过去**一次**；那边也排队 → 直接 429（带 `retryAfterSeconds`），日志不再刷屏。

**验证**：新增 `tests/_test_issue83_failover_loop.py`（11 条：三区解析、None 解析、独占模型、无就绪账号、探测异常、两处源码守卫）；全量 **60 suites / 1785 checks / 0 failed**。

## v1.6.6

**🐛 问题修复**

- **面板「智能体配置」的默认模型下拉恒为空**（#81，PR #82 by @turmony）：`GET /agents/models` 恒返回 `{"models": []}`。两处缺陷叠加：
  1. `gateway_models_for_agents()` 把 `fetch_models_merged()` 的返回值当 `dict` 解析，而它返回的是 `(mid, meta, realm)` **元组**，`isinstance(e, dict)` 全判负；
  2. 带 `realm` 时调用 `fetch_models_merged(realm=...)`，该函数没有这个参数 → `TypeError`，被外层 `except` 吞掉。
  现在：不带 `realm` 用两区并集，带 `realm` 用 `fetch_models(realm=...)`，再统一经 `model_entry()` 展开（与 `/v1/models` 同源），取 `id` / `context_window` / `max_output`。
  实测：`/agents/models` **0 → 19** 个模型、`?realm=cn` **0 → 14**；与 `/v1/models` 的 id 集合逐一对齐。

**🧪 测试**

- 新增 `tests/_test_issue81_agents_models.py`（20 条）：三种调用非空、id 与 `/v1/models` 一致且不重复、`context_window`/`max_output` 与 `model_entry` 一致、源码守卫禁止 `fetch_models_merged(<实参>)`。

**验证**：合并后全量 **59 suites / 1774 checks / 0 failed**。

## v1.6.5

**🐛 问题修复**

- **Anthropic 入口的结构化工具历史恒为 False**（#79，PR #80 by @rongyuio）：#76/PR #77 让三个入口在「未指定出口」时传 `None`，但 `_handle_anthropic` 的**形态决策**也拿到了这个 `None`——而 `structured_tool_history_enabled()` 的 auto 分支要求 `realm == "cn"`，`None` 直接判负，白名单（`deepseek` / `dfmodel` 等）永远不生效。结果是每个 Anthropic 请求都回退文本化，模型重新看到 `LEAK_MARKER` 并按 #49 的方式复述成正文。
  修法：形态决策用与 `open_upstream` **同源**的兜底（`req_realm or detect_model_realm(model) or CURRENT_REALM`），但**不改动** `req_realm` 本身（它要作为 `target_realm` 传给路由，改了会跳过自动选路）。新增 `tests/_test_issue79_structured_realm.py`（源码守卫 + 真值表 + 金丝雀）。
- **失败行缺账号 / Key / 流式字段**（PR #78 by @cliffcat）：`record_error` 一直没有 `stream` / `account` / `key_id`，失败行进 `usage.jsonl` 时三项恒为空，面板只能显示「账号=-、Key=历史行未记录」。现在 21 个调用点全部回填；**不传就一个键都不写**（历史行格式不变）；选号失败时 `account` 保持空（不猜）。
- **`vm_status` 来源谎报**（PR #78）：桥不可用 + 物理机时结论来自本机交叉校验，却被标成 `runtime-info`；反向的「桥可用但无 vmInfo 却写桥不可用」也一并修掉，现在按三种情况分别措辞。

**🧪 测试**

- **测试不再污染线上用量**（PR #78）：`_test_qoder.py` 的环境变量名写错（`USAGE_DIR` vs `QD_PROXY_USAGE_DIR`）、`bootstrap()` 只隔离账号目录、4 个套件直接 import 绕过 `bootstrap` —— 现在由 `run_all.py` 统一注入用量目录，并在跑前清理 `_acc/usage`。实测跑前后仓库 `usage/usage.jsonl` 的 md5 不变。

**验证**：PR #80 独立 57 suites / 1740 checks；PR #78 独立 57 suites / 1737 checks；合并后全量 **58 suites / 1754 checks / 0 failed**。

## v1.6.4

**✨ 新增功能：本地 web 工具（默认关闭）**

客户端（Codex 等）在请求里声明的内置 `web_search` / `web_fetch`，Qoder 上游不会执行——此前被直接丢弃（#63），客户端以为能联网、实际永远调不到。现在可以交给网关**本地执行**：

1. **声明改写**：把 `web_search` / `web_search_preview` / `web_fetch` 换成网关自己的同名 function；
2. **本地执行**：搜索走 DuckDuckGo → Bing → Wikipedia 轮换（含 TTL 缓存、single-flight、节流、被封冷却），`web_fetch` 抓模型给出的 URL；
3. **结果回灌**：把结果作为 tool 消息追加，再开一轮上游继续生成，最多 3 轮（`QD_WEB_TOOLS_ROUNDS`）。

**安全边界**（这是唯一由模型驱动出网的能力）：

- 只允许 `http/https`，拒绝带凭据的 URL、`localhost`、私网 / 链路本地地址（含 `169.254.169.254`）；
- 重定向**逐跳复检**，重定向到内网直接失败；
- 响应体 4MB、正文 20000 字上限，固定超时；
- 配置代理时由代理做 DNS 解析（仍拦字面私网地址）。

**开关**：看板「设置 → 本地 web 工具」复选框（持久化在 `accounts/settings.json` 的 `web_tools`，改完立即生效）；`QD_WEB_TOOLS=on/off` 环境变量优先。**默认关闭**。

**代价**：每代跑一轮都会多消耗一次账号额度。

**验证**：新增 `tests/_test_web_tools.py`（24 条：SSRF 七种拒绝形态、声明改写、开关优先级、工具执行、接线守卫）；全量 **56 suites / 1725 checks / 0 failed**。

## v1.6.3

**✨ 新增功能：用量洞察（移植 wb 的三项能力）**

- **费用估算**：按「缓存命中 / 未命中输入 + 输出」三档单价（USD / 1M tokens）把每条请求折算成等价金额。
  - `GET /pricing` 返回价目表与**未定价模型清单**（未收录的显示 `—`，绝不猜数字）；
  - 单价可用 `usage/pricing-overrides.json` 覆盖（支持 `{in,cached,out}` 或三元数组），改完 30 秒内生效；
  - `/usage` 快照新增 `cost_usd` 与 `unpriced`，面板「用量洞察」卡片显示合计；
  - 口径说明：**参考 OpenRouter 公布价**，不等于官方积分，不能用于结算。
- **积分获取记录**：`GET /accounts/credits/grants` 把各账号积分包摊平成一张表（名称 / 面额 / 剩余 / 已用），只读内存快照、不发上游请求。
- **剩余用量估算**：`GET /usage/remaining` 给出每账号「剩余积分 / 近 24h 消耗 / 近 24h tokens / 预计可用天数」，按近 24h 速率线性外推（仅参考）。
- **签到 / 领取动作流水**：新增 `qoder_activity`，`POST /accounts/checkin` 与领取动作会往 `usage/activity.jsonl` 追加一行（时间 / 动作 / 账号 / 结果 / 积分 / 说明），5MB 自动轮转、读接口保留 30 天；`GET /accounts/activity` 按时间倒序返回并给出成功/失败/累计积分汇总。

**🎨 面板**

- 新增「用量洞察」区块：等价花费卡片（含未定价数量）、积分包卡片、最快耗尽账号卡片，以及三张明细表（积分包 / 剩余用量 / 动作流水），30 秒自动刷新。

**📚 文档**：`docs/api.md` 增补四个端点与计价口径、覆盖文件格式。

**验证**：新增 `tests/_test_insights.py`（23 条）；全量 **55 suites / 1701 checks / 0 failed**。

## v1.6.2

**🐛 问题修复**

- **未指定出口的请求被入口层钉死当前区**（#76，PR #77 by ZaneCJP）：三个请求入口都写成 `self._request_realm() or CURRENT_REALM`，把「用户没指定出口」折叠成「当前区」——于是区域独占模型在 `_model_precheck` 阶段直接 400，`open_upstream` 里基于模型的自动路由分支永远进不去。现在三个入口一律传 `None`，由路由层按模型/账号自动选；`X-Realm` / `?realm=` / Key 绑区的显式优先级逐字节不变，`GET /v1/models` 的两个端点仍保留确定区域。新增 `tests/_test_issue76_auto_realm.py`（21 条 + 金丝雀自证）。

**🔧 身份形态对齐（PR #75 by maiphucguy）**

对照官方 **桌面端 0.4.8 / qodercli 1.1.75** 静态核对后修正出站形态：

- **推理链路**：UA 改为官方 CLI 同款 `undici`（此前是 `Go-http-client/2.0`，等于自报脚本）；`cosy-version` 升到 `1.1.75`（可用 `QD_COSY_VERSION` 覆盖）；补 `cosy-business-product` / `cosy-business-type` / `cosy-scene`，`cosy-data-policy` 改小写；`cosy-machineos` 所有请求都带。
- **签到 / 活动链路**：请求头改为官方桌面端的精确集合（不再叠 `X-Request-ID` / `Origin` / `Referer` 等）；`Cosy-MachineHostname` 取本机主机名（按官方规整规则），`Cosy-MachineOS` 用真实的 `{arch}_{platform}`，`Cosy-Version` 升到 `0.4.8`；runtime-info 环境码 cn=`0` / intl=`3`，且只有 darwin/win32 带 `--account-stdin`。
- **UMID 组件发现**：覆盖三端默认安装目录（Windows launcher/Programs/ProgramFiles、macOS `.app`、Linux `/opt`）；Windows 未装桌面端时回退 `QD_UMID_DIR` 或仓库 `umid/`；`_install_umid.py` 支持 `--platform win32`（按 PE 的 DLL 标志位区分 exe/dll）。
- **身份缓存**：落盘缓存记录 `environment`，缺字段或与当前环境码不符时作废重取。

**⚠️ 升级影响（务必看）**

1. **身份会重新获取一次**：旧缓存没有 `environment`，升级后首次走活动链路会重取并写回，之后照常复用。
2. **会上报主机名**：签到/活动请求带本机主机名（与官方桌面端一致）。Docker 默认主机名是容器 ID，每次重建会变；想固定请在 compose 里设 `hostname:`。
3. **Linux / Docker 的 `Cosy-MachineOS` 会变**：从 `x86_64_win32` 变为 `x86_64_linux`（与推理链路一致）。想保持旧指纹可设 `QD_COSY_MACHINE_OS=x86_64_win32`；设为 `auto` 则按系统架构自动识别。
4. **推理请求 UA 变化**：`Go-http-client/2.0` → `undici`。

**验证**：PR #77 独立 54 suites / 1662 checks；PR #75 独立 53 suites / 1656 checks；合并后全量 **54 suites / 1678 checks / 0 failed**。

## v1.6.1

**🐛 问题修复**

- **自定义签到时间保存报 `unknown account endpoint`**（#70 后续）：`/scheduler/config` 落在 `/accounts` 前缀路由里，却没在 `_handle_accounts` 中注册，最终被兜底成 404。已在账号路由里补上（与 `/scheduler/trigger`、`/scheduler/toggle` 同处），并删掉 `do_POST` 里那份不可达的副本。

**🎨 体验优化**

- **排程改成「24 小时刻度圆环」选时刻**（#70 后续）：不再手写 `09:00,21:00`，直接点圆环上的整点选中/再点取消；签到与保活分开两个页签，环心显示已选数量，下方实时汇总「签到：… 保活：…」。cron 收进「高级」折叠项，脚本用户仍可用。
  - 纯 SVG 绘制（24 段扇形 + 0/6/12/18 刻度），无第三方依赖；
  - 保存时由选中集合生成时刻串，最多 24 个整点；空选会提示而不是提交空值。

**验证**：全量 **53 suites / 1642 checks / 0 failed**（`_test_issue70_schedule.py` 扩到 27 条，新增圆环/路由断言）；另做了真实端到端：起一个临时网关 → 面板登录 → `POST /scheduler/config` 返回 200 并回显新时刻与时区。

## v1.6.0

**🐛 问题修复**

- **账号备注保存后 15 秒消失**（#74）：#69 只给 `to_dict()`（写文件/导出）加了 `note`，面板数据源 `public()` 漏了——保存后前端手动补一次能看见，15 秒轮询覆盖后就没了。现已补上；顺带把容器里必然出现的 `local model catalog read skipped` 从 WARN 降到 DEBUG（无官方客户端属正常降级）。
- **残缺工具回声被吞后按 stop 收尾，coding agent 误判任务完成**（#72）：模型把工具调用退化成只有一个 18 字符标记时，旧逻辑按 #9 静默吞掉并以 `finish_reason=stop` 正常结束——agent 以为做完了，会话在长任务中途静默终止。现在**声明了 tools 且只回 marker** 时改为 fail-closed（新增 `EmptyToolEcho`，继承 `InvalidToolArguments`，四条协议出口自动复用：chat 流式 error 帧 / Responses `response.failed` / Anthropic `event: error` / 非流式 502），客户端据此重试；未声明 tools 时仍按原样丢弃（不再把孤立标记透传给用户）。截断数组维持 #9 的丢弃语义不变。
- **信封层排队仍冷却账号、计数越界、不跨区、最终 403**（#71）：
  - 排队（10605）在解绑会话/冷却**之前**短路：不冷却、不解绑、不判死（单号池不再被打成「no usable account」）；
  - 日志计数改为 `min(attempt, budget)/budget`，不再出现 `try 4/3`；默认排队轮次 **6 → 3**（≈90s，现场 6 轮会被拖到 3.5 分钟）；
  - **跨区 failover**：排队耗尽且另一区同模型有就绪账号时自动切区（尊重显式 `X-Realm`）；
  - 排队最终统一返回 **429 + Retry-After**（按上游 `retryAfterSeconds`），不再把 403 丢给客户端。

**✨ 新增功能（#70）**

- **排程可配置**：签到/保活时刻改为面板可改（`09:00,21:00` / `22:00`），支持 **5 段 cron**（覆盖签到时刻）、账号间隔抖动；新增 `POST /scheduler/config`，状态里显示当前时区。
- **排程不再漏命中**：旧实现靠「每分钟轮询 `cur_min == 0`」命中整点——容器时区与宿主机不同、或上一轮巡检跑过 1 分钟，整点就整段跳过（用户看到的就是「签到无效」）。现在改成「到点即补」：每 15s 检查是否有已过点但未触发的排程，最多回补 3 小时内的漏点。

**验证**：新增 `_test_issue70_schedule.py`（21 条）、`_test_issue71_queue_envelope.py`（12 条）、`_test_issue72_empty_echo.py`（14 条）；全量 **53 suites / 1636 checks / 0 failed**。

## v1.5.14

**🐛 问题修复**

- **排队 403（10605）被当成「凭证被拒」，面板看起来像 Key 失效**：上游排队超过等待预算后返回 **HTTP 403 + `{"code":"10605","isQueued":true,"retryAfterSeconds":30}`**（双层转义）。旧逻辑走 401/403 分支 → `note_error(cooldown=60)` 冷却账号 → 单号池直接变成 `no usable account`，界面上就像「Key 无效」。
  现在：**排队不是账号问题**——不冷却、不判死、不解绑会话亲和，只记录一条 WARN；全部账号都排队时抛 `RateLimited` 并按上游 `retryAfterSeconds` 回 `Retry-After`（客户端可据此退避）。真 `TOKEN_EXPIRE` 仍按原语义禁用账号。
- **排队等待预算改为「请求级」**：此前 `queue_tries/queue_start` 在账号循环内重置，N 个账号各等满 6 轮，单请求会被拖到 3N 分钟（现场实测 197s）。现在整次请求共享 `QD_QUEUE_MAX_ROUNDS` / `QD_QUEUE_WAIT_BUDGET_SECS` 预算。
- **错误体读空导致排队判定失灵**：多轮重试时同一异常的 body 可能已被读走，分类阶段 `detail` 为空 → 退回「凭证被拒」。现在回退取 `exc.qoder_detail`。

**✨ 新增功能**

- **账号 / API Key 备注**（#69）：
  - 账号卡片新增 **备注** 按钮（`POST /accounts/note`，面板会话鉴权，最长 120 字），备注随账号文件持久化并在卡片上显示；
  - API Key 编辑表单新增「备注（可选，长说明）」字段，随 `/settings/save` 持久化并在 Key 卡片上显示（原「备注名称」保留为短标签）。
  - 两个字段都**不参与**任何路由/计费/风控判定，纯展示。

**验证**：新增 `tests/_test_queue403.py`（12 条）与 `tests/_test_issue69_notes.py`（17 条）；全量 **50 suites / 1589 checks / 0 failed**。

## v1.5.13

**✨ 新增功能**

- **一键更新**：看板「运行信息」检测到新版本后出现 **一键更新** 按钮，点击后网关自己完成
  「下载官方 release → 编译校验 → 备份 → 替换 → 自动重启 → 页面自动刷新」。
  - 来源固定为 GitHub 官方 release 的源码 tarball，不接受任意 URL；解压先过滤路径穿越；
  - 替换前核对 `VERSION` 必须等于目标 tag，且 release 内所有 `.py` 都能编译；
  - 覆盖前把本次会动的文件备份到 `backups/update-<旧版本>-<时间戳>/`，任一步失败**自动回滚**；
  - `accounts/`、`python/`、`backups/`、`.git/`、日志与 PID **永不覆盖**；
  - 更新完成后按原启动参数拉起新进程（`os._exit` 交棒），页面轮询 `/health` 自动刷新。
  - 安全边界：端点要求面板会话、默认**只允许本机**触发；`QD_UPDATE_DISABLE=1` 可完全禁用；
    容器内默认拒绝自更新并提示改用 `docker compose pull`。
  - 新增 `GET /update/plan`（预检：当前/最新版本、是否容器、是否禁用）与 `POST /update/apply`。

**验证**：新增 `tests/_test_update_oneclick.py` 21 条（文件选择、保护路径、版本/语法校验、解压穿越防护、容器拒绝、端点守卫、面板接线），并做了本地假 release 的端到端替换验证（版本变更、备份存在、`accounts/` 保留、新文件同步）。全量 **48 suites / 1560 checks / 0 failed**。

## v1.5.12

**🐛 问题修复**

- **同一组织多账号的共享额度池被重复累加**（#67，报告者 YuEr159）：`/credits/summary` 逐账号累加 `remain_effective`，而组织共享池在**每个账号**的 quota 响应里都会回显一份 → 5 个账号把 1,000,000 乘成 4,000,000。现在汇总按 `organization_id` 去重（同组织只算一次），无限池不计入数字、单列 `unlimited_orgs`；行内新增 `credits_counted` / `credits_org` / `credits_shared_unlimited`，原 `credits_remain_effective` 保留。
- **`cap: -1` 被当成真实余额**（#67）：`cap` 为负是「不设上限」的哨兵，`remaining` 只是占位（`used` 恒 0、各账号回显一致）。现在标记 `shared.unlimited`，占位值**不再**计入 `remain_effective`；余额地板护栏（G1）与 402 解封对无限池改为**显式语义**（不拦 / 直接解封），不再靠「数字很大所以碰巧没触发」。
- **413 友好文案从未生效**（PR #65 by maiphucguy）：`friendly_upstream_error` 里 #59 新增的分支被写成了一行带字面量 `\n` 的注释，整段是死代码；对应断言恒为真。已恢复为真实代码，断言改为严格判断 `type == payload_too_large`。
- **请求自身的 4xx 让全池冷却 60 秒**（PR #65）：内容审核 / 参数非法 / 上游 413 这类「换号结果不变」的错误，此前会逐号冷却换号，期间其他请求直接 503。新增 `_is_request_fault()`：命中时不换号、不冷却，原错误直接抛给客户端（`authentication_error` / `permission_error` 仍走轮换）。
- **全池同样失败会熔断 30 分钟**（PR #65）：断网 / 上游整体故障这类与账号无关的失败，此前每请求给每个账号各记一次，3 次即熔断，且熔断期间没有成功请求来复位。改为**延迟记账**：只有「同一请求换号后成功」才给先前失败的账号记 `note_failure` / `note_unknown_failure`；全池同样失败则不计入（日志可见）。短冷却、429 软限流、401/403、团队租约、风险遥测行为不变。
- **模型文案内嵌快照自 v1.1.0 冻结**（#68，报告者 YuEr159）：`load_official_text()` 只扫 Windows 安装目录，Linux / 容器（home=/root）永远匹配不到 → 静默回退内嵌快照，快照里还写着早已下线的 `DeepSeek-V4-Flash-0731` / `V4-Pro-0813`。现在：
  - 内嵌快照按 Qoder 现行文档重写，**不再带版本号后缀**；`kmodel` label 与上游清单对齐（Kimi-K2.8-Preview）；
  - 新增 `QD_OFFICIAL_TEXT=/path/to/qoder-cn.json`（文件或目录）显式覆盖，并扫描 `/app/assets/dynamic-text` 等容器挂载点与 Linux/macOS 客户端位置；
  - 回退内嵌快照时打一条 WARN（只一次），不再静默。

**📚 文档**

- **README 重写与拆分**（PR #66 by maiphucguy）：1737 行压到 103 行（特性 / 快速开始 / 客户端接入 / 文档索引），其余按主题拆到 `docs/`（deployment / configuration / internals / accounts / clients / models / api / development / troubleshooting），发布说明移入 `CHANGELOG.md`；徽章改为动态 release / CI 状态。PR 里还逐条核对了 **11 处「README 与代码不一致」**（intl 网关顺序、机器头回退、`/models/refresh` 鉴权、推理 UA 仍是 Go 默认值、`thinking=disabled` 并不真正关闭思考等），其中属于代码问题的几项留待后续。

**验证**：全量 **47 suites / 1539 checks / 0 failed**（新增 `_test_issue67_org_shared.py` 13 条、`_test_issue68_model_text.py` 10 条，PR #65 自带 11 条）。

## v1.5.11

**🐛 问题修复（zcode 工具调用仍被截断或漏成正文）**

- **`dfmodel` 没进结构化白名单**：DeepSeek 的上游 key 是 **`dfmodel`**（显示名 DeepSeek-V4-Flash），白名单只写了 `deepseek`，匹配不到，auto 模式一直走文本化，模型照抄伪协议复述。现已加入，这类会话直接走结构化直传，从源头不再产生可复述的伪格式。
- **标记的括号变体**：`[<assistant 请求调用工具]`、缺尾括号、多一层括号等写法统一识别，不再「换一种括号就漏一次」。
- **正文与标记落在同一个大块**：此前整块以正文开头时会被判定为「不是回声」直接透传；现在先放行正文前缀，只把从标记开始的部分继续当作候选。
- **逐字节守恒**：归一化只规范化**已存在**的括号，不再给缺尾括号的片段补字符，修掉跨帧拼接多出一个 `]` 的回归。

**验证**：新增 `tests/_test_issue60_followup.py`（17 条，覆盖 1 / 8 / 64 / 整段分帧、正文同帧、双括号守恒、未声明的工具名不恢复）；全量 **44 suites / 1498 checks / 0 failed**。

## v1.5.10

**✨ 新增功能**

- **流量守卫（对所有账号生效）**：`QD_TRAFFIC_GUARD=on` 把 #56 的团队闸门扩展到全部账号——单号并发默认 **1**、最小间隔 **400ms**，避免「自己把号打到并发过高」触发限流；可调 `QD_TEAM_MAX_CONCURRENCY` / `QD_TEAM_MIN_GAP_MS` / `QD_TEAM_WAIT_SECS`。见[团队与企业账号](docs/accounts.md#团队与企业账号)。
- **风控遥测**：`GET /health` 新增 `risk` 字段（`queue` / `rate` / `rejected` / `version_gate` 计数与最近一次信号）；`scripts/risk_diag.py` 只读诊断账号健康、生效中的开关、容器 + 国际版告警与日志信号。

**🐛 问题修复**

- **国际版出口顺序对齐官方**（逆向国际版客户端的 endpoint 缓存）：推理主机改为 `api3.qoder.sh`，`api2` / `api1` 作为备用。旧配置以 api1 为主，而官方 0.4.8 的候选首位早已是 api3（本机实测 api1 常年超时）。可用 `QD_INTL_GATEWAY` 覆盖。
- **推理请求头补齐官方形态**：SSE 请求新增 `cosy-machineos` 与 `cosy-machinehostname`（后者官方只在 infer-sse 携带，按账号稳定派生、不同账号互不相同）；`QD_COSY_OS_HEADERS=0` 可关闭。
- **排队不再换号**：10605 是模型级队列，换号不会变快；现在排队时**不解绑会话亲和**，避免「同一会话在多个账号间跳」的异常画像。
- **430 版本门可读化**：返回「客户端 / 网关版本过低，请升级」，不再被误判成风控或账号问题。

**📚 文档**

- [风控处置手册](docs/risk-control-playbook.md)：分层处方（出口 > 身份 > 调度 > 请求 > 观测 > 应急）、容器 / 数据中心的三种落地方案、只做软件层时能控制什么、P0 / P1 / P2 路线与验收指标。

**验证**：全量 **43 suites / 1478 checks / 0 failed**。

## v1.5.9

**🐛 问题修复 / 防风控对齐（PR #61 by ddddd-ren）**

三处出站身份形态与官方客户端对不上，都可能被动聚类成「网关特征」：

- **出站 UA**：账号相关请求（`headers()`、Token 刷新、设备授权取 userinfo、PAT 导入的 jobToken 交换）此前用的是 `Go-http-client/2.0`。现在按区域取官方 UA：国内版 `QoderWork/1.1.64`，国际版 `Qoder/1.1.64`。
- **请求 ID**：`X-Request-ID` 此前是「uid 派生的固定前缀 + 微秒」，那个前缀是每个请求都带的稳定标记。现在改为每个请求纯随机的 32 位 hex。
- **设备码形态**：`derive_id` 从「32 位无连字符 hex」改为 **UUIDv4 形状**（与官方 machineId 同形）。派生来源（uid + salt 加盐哈希）不变，同一账号恒定、不同账号隔离。

**⚠️ 存量号池注意**：设备码形态变化后，上游会把每个已入池的账号看成「换了一台设备」。想避免这次换设备，升级前设置 `QD_DEVICE_ID_LEGACY=1`（保持旧的 32 位 hex 形态；该开关只影响格式，不影响派生来源）。新号池不需要设置。

**⚙️ 行为调整（排队预算对齐官方）**

- 排队等待从「3 轮 × 30 秒 ≈ 90 秒」提升到**分钟级**：`QD_QUEUE_MAX_ROUNDS` 默认 **6**，`QD_QUEUE_WAIT_BUDGET_SECS` 默认 **600**（10 分钟），单轮仍受 `QD_QUEUE_WAIT_MAX_SECS`（180 秒）约束。
- 每次等待加 **0–2 秒抖动**（`QD_QUEUE_WAIT_JITTER`），避免多个客户端同时被唤醒、一起打同一个队列。

**📚 文档**

- 新增[风控与排队](docs/risk-control-and-queueing.md)：上游看到的维度（客户端指纹 / 设备身份 / 账号 / 网络 / 行为）、处置等级（10605 排队、403、429、418、内容审核）、多账号为什么仍会排队、分层缓解办法与反模式；以及对官方桌面端的只读逆向结论——排队走独立轮询协议 `GET /api/v2/service/ask/queue/status`，总等待默认 60 分钟（`QODER_MODEL_QUEUE_MAX_WAIT_MS`）；推理头为 `Cosy-Version` / `Cosy-ClientType` / `Cosy-MachineOS` / `Cosy-MachineHostname`；设备身份由原生 SDK 计算；客户端专门处理的错误码 `10605 / 114 / 430 / 100401 / 100403 / 80411 …`。

**验证**：全量 **42 suites / 1460+ checks / 0 failed**（PR 自带 17 条 + 本地补 5 条形态 / 开关断言）。

## v1.5.8

**🐛 问题修复**

- **Codex / MCP 的 `namespace` 工具导致上游 400**（#63）：Codex 会在 `/v1/responses` 的工具列表里发 `type: "namespace"`（MCP / multi-agent）与 `web_search` 这类 Responses 专有类型。网关原样透传给 Qoder，上游在反序列化阶段直接报 ``unknown variant `namespace`, expected `function` ``，整个回合失败（稳定复现，不是网络抖动）。
  现在两处都做过滤：`_tools_for_chat()`（Responses → Chat 转换）与 `normalize_tools()`（Chat 请求规范化）只保留 `function` 与已降级的 `custom`，其余类型丢弃，并按类型节流记一条 WARN（同类每 10 分钟一条）。`function` 工具与 custom 降级逻辑完全不变。
  注：namespace / MCP 工具本身**仍然不会被调用**，因为上游无法表达它们。

**验证**：全量 **42 suites / 1442 checks / 0 failed**（`_test_issue56_62.py` 扩到 31 条，覆盖工具过滤与告警）。

## v1.5.7

**🐛 问题修复**

- **Anthropic 入口丢弃 Claude Code 的 adaptive 思考档位**（#62）：Claude Code 对**第三方模型名**发送的是 `thinking={"type":"adaptive"}` + `output_config={"effort":"low|medium|high|max"}`。adaptive 是合法的 dict，但没有 `effort` / `budget_tokens`，旧实现因此整段跳过 `output_config` → 四档全部映射成 `None` → 上游按默认档位（多数模型是 `max`）运行。选 low / high 都白选，而且表现为「更费 token、更慢」却不报错。
  现在 `thinking` 里取不到档位时回退读取 `output_config.effort`；`disabled` 仍然生效。
- **`budget_tokens` 够不到 max**：断点表原来最高只到 `high`（大于 16384 一律 high），补上一档：`≤32768 high`，更大为 `max`。

**✨ 新增功能**

- **团队 / 企业账号「自用保护」**（#56）：只对识别为团队号的账号生效，把**能由客户端行为避免**的风控点先避开：每账号并发闸门、请求启动间的最小间隔与抖动、客户端 IP 统计告警。拿不到租约时等待后放行，不会卡死。详见[团队与企业账号](docs/accounts.md#团队与企业账号)。

**验证**：全量 **42 suites / 1436 checks / 0 failed**（新增 `_test_issue56_62.py` 25 条）。

## v1.5.6

**🐛 问题修复**

- **多模态请求出站体积膨胀 3 倍，超过 30MB 限制后被 RST**（#59）：`build_qoder_body` 在 `messages` 的结构化 parts 之外，又把整份 Base64 **重复塞了两处**（`body.image_urls` + `chat_context.imageUrls`）。上游只读 parts，这两份纯属冗余：10 张图 15MB 会变成 48MB，编码后约 64MB，超过阿里云 SLB 的 30MB 限制被强行断开（Broken pipe）；Broken pipe 又被当成瞬时故障反复重试，最终把整个号池打进熔断。现在默认不再填这两处（`QD_KEEP_TOPLEVEL_IMAGES=1` 可恢复旧行为）。
- **出站体积前置保护**：编码后超过 `QD_MAX_BODY_MB`（默认 29MB）时**就地返回 HTTP 413**，不发网络请求、不轮询账号、不写冷却。
- **zcode 工具调用漏成正文**（#60）：模型把内层 `arguments` 的引号**未转义**地写出来时，外层数组也不是合法 JSON，此前四条回读路径全部拒绝，整段协议被当成正文下发。现在增加「抢救式解析」（补齐内层引号转义后，按同一套严格校验重建），并在流式暂存阶段对「数组中途的 delimiter 错误」保持候选，不再中途否决。

**✨ 新增功能**

- **模型清单磁盘缓存 + 手动刷新**（#58）：动态发现成功的清单会落盘（`accounts/models-cache-<区域>.json`，7 天有效），容器重启或上游抖动时先用上次的结果，不再回退到陈旧的内置快照；新增 `GET /models/refresh`（需要 API Key）强制重新发现。

**⚠️ 提示**

- **不要在容器里反代国际版**（#57）：数据中心出口访问国际版网关容易被风控（表现为聊天排队）。检测到「容器 + 国际版账号」时，启动日志会打印 WARN；建议使用住宅出口，或改用国内版。

**验证**：全量 **41 suites / 1411 checks / 0 failed**（新增 `_test_issue57_60.py` 19 条）。

## v1.5.5

**🐛 问题修复**

- **Anthropic 入口把动态 system 抬进缓存前缀，命中率 92% → 14.7%**（#55）：`system_prompt_of` 会把 `messages` 里的 system 合并进最前面的 system，而客户端（Claude Desktop/Code）恰恰把**每轮都变**的内容放在那里（当前时间、工具清单、skills、context 用量）。合并后前缀逐轮不同，上游缓存从第 0 字节就失配。
  现在默认 **inline**：`messages` 内 system **就地降级成一条 user**（内容不丢、位置不变），顶层 system 保持字节稳定；旧行为保留为 `QD_ANTHROPIC_MSG_SYSTEM=merge`，另有 `drop`。
- **指纹看不出来源**（#55）：`usage.jsonl` 新增 `sys_src`（`top` / `top+inline` / `top+msg` / `none`）与 `n_msg_system`，下次这类问题不必翻代码。
- **智能体一键配置根本发不出请求**（#53，PR #54 by WCX0221）：`/settings` 只回掩码、从不回明文 `key`，而下拉用 `k.key` 做 option value → 所有 option 值都是空串 → 点「一键配置」只弹「请先选择 API Key」。现在 option value 用 key **id**，点击时经 `/settings/reveal?id=` 取一次明文（与设置页复制 Key 同一路径）。

**⚙️ 行为变更**

- **DeepSeek 进入结构化工具历史白名单**（#49 后续）：报告者在真实长会话（Anthropic 入口、1,283 次工具调用）验证 **0 降级**，同环境开启前基线 0.30%。结构化直传后模型看不到 `LEAK_MARKER` 文本格式，从根上消除复述。`_tool_ids_ok` 守卫仍在（id 不齐备自动回退文本化），INTL 仍不在 auto 范围。

**验证**：全量 **40 suites / 1392 checks / 0 failed**（新增 `_test_issue55_system_prefix.py` 17 条 + PR 自带 `_test_agents_panel.js` 20 条）。

## v1.5.4

**🐛 问题修复**

- **信封层排队（10605 / RESOURCE_LIMIT）没有等待**（#51 后续）：上游会先以 HTTP 200 建流、再在 SSE 信封里投递 `403 {isQueued:true, retryAfterSeconds:30, subCode:RESOURCE_LIMIT}`。v1.5.3 之前只在 **HTTP 层**做了排队等待，信封层仍按普通 403 冷却换号，换不到就把 403 直接丢给客户端（表现为 `request error: status=403 … 10605`）。
  现在信封层同样识别排队：`should_retry_envelope` 对排队信号允许比瞬时重试更多轮（最多 `QUEUE_MAX_ROUNDS`），重开前按 `retryAfterSeconds+1` 等待；非流式聚合循环也留出了排队轮次；普通 403 的重试轮次**不变**。
- **排队失败给可读文案**：10605 / RESOURCE_LIMIT 不再是一段嵌套 JSON，而是「上游排队中…已按 retryAfterSeconds 等待并重试，仍拿不到可用资源，请稍后重试」。

**验证**：全量 **38 suites / 1353 checks / 0 failed**（新增 8 条：嵌套 detail 解析、信封排队识别、重试轮次、等待时长、普通 403 不变、已吐字节不重开、文案映射、聚合轮次）。

## v1.5.3

**🐛 问题修复**

- **DeepSeek 原生 DSML 在 chat / Anthropic 流式里完全没有回读**（#51）：此前只有 Responses 路径有 DSML 解析，模型直接吐 `<|DSML|invoke name=...>` 时整段协议漏成正文（截图三例：闭标签+数组、完整 DSML 块、marker+杂字+数组）。现在流式出口新增一层 DSML 缓冲：遇到 DSML 就压住到流末统一解析，成功则补发散文 + `tool_calls` + `finish=tool_calls`；解析不出则原样透传并记 WARN。非流式 chat 侧也补了同样的回退。三种形态在 1/8/64/整段分帧下都能恢复、零泄漏。
- **闭标签/杂字形态的解析容错**：允许 `</|DSML|...>` 前导闭标签、允许 marker 后跟杂字（如 `">`) 再进数组、允许数组开头的 `[` 丢失（取首个 `{` 到末个 `}` 重新成组）；严格校验（name 非空且命中声明、arguments 合法）一律保留。
- **连接被拒绝只显示裸异常**（#52）：`<urlopen error [WinError 10061] ...>` 现在翻成可操作中文——点名目标地址、并给出三步排查（服务/端口、环境变量代理是否还在跑、地址是否写错）；10060 超时与 10054 重置同样处理。

**✨ 新增功能**

- **关掉窗口不再停服 + `stop-qoder-proxy.bat`**（#50）：`start-qoder-proxy.bat` 改为 `Start-Process` 分离启动（隐藏窗口、日志落 `qoder-proxy.log`），关掉启动窗口服务继续跑；新增 `stop-qoder-proxy.bat` 按 `qoder-proxy.pid` **精确停止**（先校验目标进程命令行含 `qoder_proxy.py`，绝不误杀其它 python）。服务端新增 `--pid-file`，启动写 PID、退出自动删除。

**验证**：全量 **38 suites / 1345 checks / 0 failed**（新增 `tests/_test_issue50_51_52.py` 25 条）。

## v1.5.2

**🐛 问题修复**

- **长会话工具调用复述：散文 + 双 marker + 数组 + DSML 尾**（#49）：模型在长会话里会连写两个 `LEAK_MARKER`、前面带散文、后面跟 DSML 闭标签，旧守卫要求「整段恰好是 marker + 数组」，于是四条路径全部拒绝、整段原样下发（`stop_reason=end_turn`，工具静默不执行，实测约 1.5%）。
  现在解析取**最后一个** marker 起算、跳过连续重复 marker、剥掉尾部 DSML/围栏；前方散文作为 `clean` 保留**且不含 marker**。流式侧同步：`_leak_body_offset` 跳过重复 marker，并对「第二个 marker 还没打完」的跨帧半截保持候选（8 字符分帧也稳）。
- **空壳循环不再完全静默**（#49 形态3）：`marker + <terminal></terminal>` ×54 这种**没有 JSON 参数**的形态无法恢复（参数根本不存在），保持 fail-open 透传；但四条路径全拒时会记一条 **WARN**（含 `markers=/dsml=/total=/suppressed=`，默认每分钟一条，避免刷屏），让发生率可观测，而不是只能靠查磁盘发现「模型说清理完成、其实没执行」。
- **流式 tool_call.id 兜底的四条边界**（#47 复核）：
  - `index` 为 list/dict/str 等**不可哈希值**时不再抛 `TypeError`（旧实现会让异常一路冒到 socket，客户端看到连接被掐断）；现在按帧内位置归一，并对整帧加了一层 fail-open 兜底；
  - **并行调用都不带 index** 时不再撞同一个 id（按帧内位置区分）；
  - **串行多个无 index 调用**（同一位置、各带 `function.name`）各得新 id，续帧沿用；
  - 中途才收到上游真 id 时**覆盖收敛**（旧 `setdefault` 会让后续帧回落到旧补值）。

**验证**：全量 **37 suites / 1321 checks / 0 failed**（新增 `tests/_test_issue47_49.py` 22 条：index 六种畸形值、并行/串行撞号、迟到的真 id、三种回声形态、流式 8/64/512 分帧、WARN 节流与严格边界回归）。

## v1.5.1

**⚠️ 行为变更（fail-closed，issue #37）**

「工具协议回声 + 内层 arguments 非法」不再以「一句说明 + 普通 stop」结束，改为**协议级失败**：

| 出口 | 行为 |
|---|---|
| `/v1/chat/completions` 流式 | `data: {"error": …}` → `[DONE]`；**不再发** stop/tool_calls 正常收尾 |
| `/v1/chat/completions` 非流式 | **HTTP 502** + 标准错误信封 |
| `/v1/responses` 流式 | `response.failed`；**不再发** `response.completed` |
| `/v1/messages`（Anthropic） | `event: error`；**不再发** `message_stop` |

动机：旧行为对按「错误帧 + 结束状态」驱动工具循环的客户端等价于**成功**（`stopReason=end_turn`、`hasFatalError=false`），工具没执行却把任务当完成。

**🐛 问题修复**

- **协议回声的围栏 / DSML 组合识别**（#37）：`_echo_with_bad_args` 此前只在「整串被围栏包住」时才剥围栏——流式里 `_find_marker_split` 先把开围栏切进散文，hold 只剩 `marker + 数组 + 闭围栏`，于是认不出、marker 原样吐给客户端。现在首尾围栏残余（含 `json` 标注）与 DSML 尾标签**交替剥两轮**再判定。
- **判定顺序**（#37）：三处出口（流式收尾 / 非流式聚合 / Responses）原先 `_leaked_partial_droppable` 在前，会把「外层数组合法 + 内层参数坏 + DSML 尾」误判成 #9 截断回声**静默吞掉**并照常 stop。现在 fail-closed 判定**提前**，与 #9 互斥（后者要求外层数组解析失败）。
- **小分块漏检**（#37）：`_json_array_prefix_ok` 的 Extra data 分支在 `tail = "```\n</"` 时既不是围栏前缀、又被 `_dsml_tail_holdable` 当成散文 → 候选在**流式中途**被证伪，1/16 字符每帧下整块当正文补发。现在 tail 以完整闭围栏开头时先剥掉再判。

**🐛 其他修正**

- 坏参数**不自动重试、不冷却账号**：`should_retry_envelope` 对该异常直接 False（避免重复计费），账号冷却/归因入口直接返回（模型内容问题不等于账号不可用）。
- 错误详情是**固定串**（不含实际参数、路径、正文）；面向客户端的文案单独给出可读中文。

**🐛 问题修复（#48 前缀缓存）**

- **历史工具结果削减随请求形态滑动，导致上游前缀缓存每轮失效**：旧规则用 `_current_turn_start` 从末尾往前找最后一条 user 消息——连续工具调用时 `_cur_start=1`（全不削），用户一插话 `_cur_start` 跳到末尾（**每条**历史工具结果全部重削）。同一条历史消息在不同轮次序列化成不同字节，上游从第一条被削处起整段不命中（实测命中率 95-99% → 8%）。
  现在默认 **`tail` 策略**：只保护**最后 1 条**工具结果，更早的一律削；一旦被削，字节里不再含随轮次变化的成分。`QD_TOOL_RESULT_SCOPE` 可选 `tail`（默认）/ `all`（全部削，缓存最稳）/ `turn`（旧行为，仅兼容）；`QD_TOOL_RESULT_KEEP_TAIL` 可调保护条数。
- **省略标记去掉字数统计**：`[[qoder-proxy: 已省略 N 字符历史工具结果…]]` → 固定串 `[[qoder-proxy: 已省略中间内容…]]`。带数字的标记会让「同一条结果」在内容变化/重试时产生不同字节，同样打断前缀。

**验证**：全量 **36 suites / 1298 checks / 0 failed**（新增 `tests/_test_prefix_stability.py` 12 条：两种请求形态字节一致、标记无数字、三种 scope、keep=0 回退）。

**⚠️ 两个契约细节**（写在这里以免误判）

- 混合批次要 fail-closed，数组里**每个** `name` 都必须在本次声明集合内；只要有一个未声明，整块仍走原有 fail-open（不把普通示例当错误）。
- 「内层真实 LF」必须落在 JSON **字符串内部**（`{"body":"line1<LF>line2"}`）；落在 token 之间的换行是合法 JSON，属「合法参数」路径。

**验证**：全量 **35 suites / 1285 checks / 0 failed**（新增 `tests/_test_issue37_fail_closed.py` 103 条）；另有 `tests/manual_issue37_e2e.py` 真起网关验证四组 HTTP 行为（200+error 帧 / 502 / response.failed / event:error）。

## v1.5.0

**✨ 新增功能**

- **面板会话跨重启存活**（PR #46 / cliffcat）：token 改为自校验签名（`<过期>.<nonce>.<hmac>`），密钥持久化在 `accounts/settings.json`；网关重启/升级不再强制重新登录，改密码时换密钥一次性踢掉所有浏览器。顺带修了三处：`settings.json` 解析失败时**拒绝回写**（避免一次瞬时 IO 抹掉密码哈希与全部 API Key）、非 ASCII token 不再打断连接、`revoke_all()` 不再持锁做磁盘 IO。
- **近 7 / 30 天 Token 用量图**（PR #46）：新端点 `/usage/daily?days=7|30&group=model|realm|key|account`，纯手写 DOM 堆叠柱状图（零第三方依赖）；日志倒读、内存 O(1)，脏行跳过，DST 时区按自然日重算。
- **独立于护栏的余额刷新**（#43）：请求线程只给账号打标（不联网），后台默认每 **5 分钟**合并刷新「真正用过的号」，空闲不轮询，用完补完最后一次；失败保留旧快照并按 60s→15min 退避重试。`QD_CREDITS_USAGE_INTERVAL` 可调。

**🐛 问题修复**

- **流式 tool_call.id 为空导致下游会话整份报废**（#47）：上游偶发缺 id 的增量帧被原样透传，客户端落盘 `id:""` 后整份会话无法加载。现在在**唯一流式出口** `recover_leaked_tool_calls` 统一兜底：有 id 原样透传、缺 id 补 `call_` 并按 index 记忆（分帧不换号），非工具帧字节级不变。
- **面板余额长时间不更新**（#43）：此前余额刷新绑在额度护栏上（阈值全 0 时线程根本不存在）。现在与护栏解耦；账号行显示「更新于 HH:MM / 待刷新 / 查询中 / 查询失败（显示旧快照）」，不再把旧快照伪装成刚更新。
- **签到积分不显示**（#40 后续）：签到成功（含 `ALREADY_CLAIMED`）后**立刻刷新余额**，奖励积分即时反映到面板。
- **国际版空额度一片空白**（#40 后续）：上游返回全 0 退化体时不再只留日志——写降级标记并在面板显示 **`0*` + 「空额度」**（悬停可见上游原因与时间），而不是「未查询」。
- **深色模式 API Key 表单白底**（#44 / PR #45）：编辑卡片内联 `background:#ffffff` 改为 `var(--panel)`。

**🎨 体验优化**（PR #46）：用量表「占比」分母改为表内可见行之和（不再最大行恒 100%）、TTFT/输入输出显式换行不再从数字中间折行、`.range-btn:hover` 蓝底深字修正；展示路径统一走 `effective_remain()`（团队账号共享额度不再显示 0）。

**⚠️ 其他变更**：新增 2 个回归套件（`_test_stream_tool_ids.py` 12 条 / `_test_credits_refresh.py` 27 条），PR #46 自带 2 个（`_test_usage_daily.py` / `_test_panel_session_persist.py`）。

**验证**：全量 **34 suites / 1180 checks / 0 failed**。

## v1.4.5

**🐛 问题修复**

- **共享额度字段名对齐**（#39 后续，报告者 realhuhu 提供了真实团队账号响应）：v1.4.4 按猜测字段名透传，实际字段是 **`orgResourcePackage`**，且用 **`cap`**（不是 `total`）、带 `available` 开关。现在：
  - 识别 `orgResourcePackage`（保留 `sharedQuota`/`teamQuota`/`poolQuota` 等兼容名），`size` 取 `cap`；
  - 账号行显示「共享 N」，`packages` 多一行「共享额度」；
  - **有效余额** `remain_effective` = 套餐 remain + 可用的共享 remain；`available=false` 时不计入；
  - **预留护栏与 402 解封改用有效余额**——团队账号「套餐用满（0/3000）+ 共享池 952」不再被误判为余额耗尽而停号；
  - `/credits/summary` 合计与面板区域卡片改用有效余额，行里同时保留原始 `remain` 便于核对。

**⚠️ 其他变更**：回归套件新增 8 条团队账号断言（报告者的真实响应逐字段：`remain=952` / `size=cap=2500` / 有效余额 952 / 护栏不拦 / `available=false` 不计入 / 汇总用有效余额）。

**验证**：全量 **30 suites / 1090 checks / 0 failed**。

## v1.4.4

**🐛 问题修复**

- **Anthropic 流式用量缺输入侧字段**（#42，报告者 zengzongxiong）：`message_delta.usage` 此前只回 `output_tokens`，按新版 Anthropic 语义（累计完整用量）读取的客户端（Claude Code / AI SDK 系）输入 token 恒为 0。现在回全 `input_tokens` / `cache_read_input_tokens` / `cache_creation_input_tokens` / `output_tokens` / `output_tokens_details`，与非流式同口径（`input_tokens + cache_read == prompt_tokens`）。
- **工具调用协议回声在内层参数非法时泄漏为正文**（#37，报告者 qingannn）：外层数组合法、内层 arguments 非法（未转义换行/引号、截断、尾部多余文本、非法转义）时，回读守卫拒绝恢复后整段协议被当正文放出并普通结束。现在识别这种形态为「协议回声 + 参数坏了」：**不泄漏协议、不伪造可执行调用**，改为一句明确的失败说明。散文/示例/未声明工具名不受影响。
- **上游排队被当成错误丢给客户端**（#38）：403 + `isQueued`/`10605` + `retryAfterSeconds` 现在会在预算内**等待队列放行后重试**（默认最多 3 轮、单轮 ≤180s、总预算 300s，均可用 `QD_QUEUE_*` 环境变量调整），不再让客户端看到排队 JSON 就直接结束。
- **国际版积分/签到 403**（#40）：403 时先用桌面端同款请求头重试一次；仍失败时给出可操作说明，并**自动探测「令牌属于另一区域」**（区域标记错误）——实测国际版 quota 接口本身可用、签到接口在 intl 不存在（404 已按「本区域无此接口」处理）；签到 403 也不再是裸错误，而是带原因的「上游拒绝访问」。
- **改密码未先校验原密码**（#41）：原密码错现在返回 **403 +「当前密码不正确」**（此前 401 会触发前端弹回登录遮罩，用户只看到登录 UI 看不到原因）。
- **共享额度不显示**（#39）：上游若返回共享额度池（`sharedQuota` / `teamQuota` / `poolQuota` / `sharedCredits`），现在纳入 `credits.packages`（「共享额度」行）并在账号行显示「共享 N」；不返回时不编造。套餐总额口径不变。

**🎨 体验优化**：账号行在共享额度存在时显示「共享 N」徽标（悬停可见已用/总量）。

**⚠️ 其他变更**：新增回归套件 `tests/_test_issues_1_4_4.py`（31 条断言，覆盖上述六个 issue 的判据）。

**验证**：全量 **30 suites / 1082 checks / 0 failed**；新套件回退四个源文件后必红（#42 五条断言现场复现），恢复后全绿。

## v1.4.3

**🐛 问题修复**

- **流式工具调用回声的三种新泄漏形态**（issue #36 后续，报告者 qingannn 二次报告并给出精确分片实验）：
  - **尖括号标记变体**：上游同一转换器还会产出 `<assistant 请求调用工具>`（方括号换尖括号）——回读守卫只认方括号，整块直接透传。修复：入口归一化（含围栏内、真前缀跨帧判定），判定逻辑仍只有一套；
  - **DSML 闭合标签跨帧**：帧边界落在标签内部（如 `</｜｜DSML｜｜ param`，无 `>`）时，尾部剥离剥不动、JSON 在 "Extra data" 处证伪 → 整段泄漏。报告者实测「16/128 字符分片失败、512 成功」正是此因。修复：尾巴判定统一为「剥掉完整标签后，剩余为空或以 `</` 进行中（含单字符 `<`）即暂留」，长度护栏 80 字符防散文误留；
  - **帧数上限误伤**：`_HOLD_MAX_FRAMES=200` 对「每帧 1 字符」的合法回声块（201+ 帧）触发 fail-open。修复：帧上限只防**空转**（连续无增长帧），有增长的块交给 32KB 字符上限兜底。
- **验证**：分片矩阵 2 种标记 × 5 种尺寸（1/4/16/64/512）全部恢复且零泄漏；报告者三个场景逐一复现转绿。

**⚠️ 其他变更**

- 测试：`_test_proxy_policy.py` P3（能红证据）在本机系统代理 bypass 含 loopback 时如实 SKIP（Windows `proxy_bypass` 读注册表 bypass 列表会让死代理对 loopback 不生效——环境不可测，SKIP ≠ PASS）；产品行为断言（P3b/P4/P5）不受影响。

**验证**：全量 **29 suites / 1050 checks / 0 failed**；新增 18 条 #36b 回归断言（归一化 4 + 分片矩阵 10 + 尾巴判定 4）。

## v1.4.2

**🐛 问题修复**

- **图像真正送达上游**（v1.4.1 修复不彻底）：实测发现 qoder 上游**不读**顶层 image_urls / chat_context.imageUrls——图像填进这两个字段后模型仍回「我没有收到图片」。改为带图消息的 content 保留结构化 image_url parts（与 workbuddy 网关同款形态），上游从此识别图像（200x200 纯红图实测模型答「红色」）；顶层 image_urls 仍照填（兼容无害）。三个入口（Chat / Responses / Anthropic）全部覆盖。
- **DeepSeek-Flash 流式工具调用偶发变正文**（issue #36，报告者 qingannn）：上游把 DSML 工具调用转成「marker + JSON 数组」后偶尔在数组尾部漏出原始 DSML 闭合标签（</｜DSML｜parameter> / invoke / calls）——残片让流式暂存缓冲在 JSON 已完成处证伪，整段被当正文补发：工具不执行、内部协议文本泄漏给客户端。修复：尾部 DSML 闭合标签剥离（仅尾部，正文内部讨论 DSML 不受影响），流式暂存与整段解析双入口生效。修复前帧序列复现必红、修复后恢复为结构化 tool_calls（finish_reason=tool_calls）。

**验证**：全量 **29 suites / 1032 checks / 0 failed**；真实链路实测（经网关 → qoder 上游）：文本基线正常、1x1 图模型确认收到（回答颜色）、200x200 纯红图模型答「红色」；#36 帧序列离线复现 + 能红证据（stash 回退必红）。

## v1.4.1

**🐛 问题修复**

- **图像单 part dict 形态被字符串化**（issue #33 反馈的根因）：部分客户端把单个 content part 对象直接当 content 字段（而非列表）发送——chat / Responses 两条路径的 str() 兜底会把整个图像 dict 字符串化成 Python 字面量文本放进消息正文，模型收到的是一段 {'type': 'image_url', ...} 文本，**图像从未真正到达模型**；Anthropic 桥的单块 dict 形态则被压成空文本静默吞掉。三入口全部改为「dict 当单元素列表处理」：图像照常收集、绝不 str()。附 4 条回归断言（chat dict / Responses dict / anthropic 单块 / 非 dict 兜底不变）。

**验证**：全量 **29 suites / 1028 checks / 0 failed**（较 1.4.0 净增 4 条图像形态回归断言）；三入口离线复现：list / dict / anthropic 单块三种形态均正确产出 image_urls。

## v1.4.0

**✨ 新增功能**

**① 智能体一键配置** —— 看板新页签「智能体配置」，把本机 AI 客户端指向本网关：
- 支持五个客户端：**Claude Code**（原生 /v1/messages）、**Codex CLI**（Responses）、**OpenCode**、**DSH (DeepSeek Harness)**、**Crush (Charm)**；本机自动探测（配置目录 or PATH）；
- **非侵入写入**：纯标准库的文本级 YAML / TOML / JSON 编辑器，只增量插入 qoder-proxy 那一段，注释、缩进、其它 provider 配置逐字节保留；模型表自动从网关拉取预填（顺带验证 Key，401 = Key 被拒）；
- **byte-exact 还原**：写入前原件字节备份（accounts/agent-backups/，每文件保留 10 份），「还原」回到接入前，网关新建的文件会被清理；多文件客户端（Codex/DSH）两阶段写入，任一失败整体回滚；
- **远程机器用 CLI**：客户端装在别的电脑时，curl /agents/cli 取配置器在客户端机器上跑（改本机配置、只从网关取模型表）；
- **安全边界**：写配置是面板会话 + loopback-only（改的是网关所在机器的文件，远程/容器按设计拒绝）；API Key 只写进客户端本地，网关侧 state 只存 sha256 指纹。

**② CI 独立测试门禁**（.github/workflows/tests.yml）—— #23（镜像漏 COPY 模块）的根因是测试只在本地跑：
- 矩阵 Ubuntu py3.9（README 宣称的最低版）/ Ubuntu py3.12 / Windows py3.12，push(main / v* tag) + 全部 PR 触发；失败套件完整日志上传 artifact；
- **发版纪律上 CI**：v* tag 必须等于源码 VERSION（带错 tag 的发版出不了门）；
- 套件跑完 checkout 必须干净（scripts/check_clean_checkout.py——往仓库写东西的套件直接红）。

**③ 深色主题 + 页内导航**（wb WebUI 模式移植，详见 🎨）。

**🐛 问题修复**

- **/v1/responses 扁平工具 400**（PR #30，daothinh）：Responses API 的工具是扁平形态（{"type":"function","name":...}），上游 chat 端点要求嵌套 tools[].function；normalize_tools() 原来只在 chat 路径跑，responses 与 Anthropic 桥接双双跳过 → 修进出站咽喉 build_qoder_body()，三条路径一次全覆盖（幂等：已嵌套的不动）；
- **容器部署安全**（wb 修过的真实事故）：容器原来不带 --lan，端口发布到宿主网络而 Key 校验默认关闭——等于把上游额度向全网段裸奔；现在强制生成随机 Key（打印在启动日志，docker logs | grep -i "api key"）；
- **测试时区夹具符号反了**（CI 首跑抓到的潜伏 bug）：_ts() 想构造「UTC+8 的 h:m」的 epoch，本地 offset 的加减号写反——本地恰好 UTC+8 时两个错误抵消所以从没红过，UTC runner 上 7 条窗口断言红 5 条；
- **#52 抽取器只读第一个 script 块**：页面新增 head 主题脚本后，第一个块是主题脚本（不含被测函数）→ 误报 missing；改为遍历全部块拼接。

**🎨 体验优化**

- **三态主题**：浅色 / 深色 / 跟随系统（右上角切换器，偏好存 localStorage，head 内联脚本防闪白；深色含全部硬编码色覆盖层）；
- **页内区块导航**：每页 section 现场生成左侧导航（标题取 h2、稳定锚点、滚动高亮、可收起、#锚点直达；窄屏转横向胶囊行）；
- **页签记忆**：记住上次页签，刷新不回首页；内容区放宽到 1800px + 自适应留白；
- **工具栏去重**：删掉与三态开关完全重叠的「⇄ 切换默认出口」按钮；9 按钮语义分组，低频全局动作收进「批量操作 ▾」；删除死代码 refreshAccounts/queryCredits；修正遗留品牌名 wb-proxy-console.log → qoder-proxy。

**⚠️ 其他变更**

- **Docker 镜像补 HEALTHCHECK**（跟随 PORT，重映射端口仍正确报告健康）；compose healthcheck 同步跟随 PORT；
- run_all.py 增加 --jobs / --logs（CI 与本地同一条命令，失败日志落可见目录）；
- 新增**整页加载层**测试套件（_test_dashboard_full_load.js：手写真 DOM 桩 eval 整个主 script 块——「切片绿 ≠ 整页绿」的补口）；.gitattributes 钉死 html/js 行尾（消除 md5 指纹噪声）。

**验证**：全量 29 suites / 1024 checks / 0 failed（含 PR #30 分支与合并后 main 各复跑一次）；CI 三平台（Ubuntu py3.9/3.12 + Windows py3.12）绿；智能体配置端到端实测（apply 增量写入 / restore byte-exact / 新建文件清理 / state 清空）。

## v1.3.7

**✨ 新增功能**

**① 双区自动路由的完整体验（#26 续）** —— v1.3.6 把自动路由发出去了，这一版把它变成**看得见的**：
- **「本次实际出口」不再靠猜**：usage 行新增 `realm_reason`（三态），面板据此给出**确定结论**：

  | `realm_reason` | 含义 | 面板显示 |
  |---|---|---|
  | `model_missing` | 当前区官方清单里没有这个模型 | 「模型不在默认出口，已自动改走 X 区」 |
  | `no_account` | 模型本区有、被**账号可用性**推走 | 「默认出口无可用账号，已自动改走 X 区」 |
  | `""`（**显式空串**） | 明确**没有换区** | 「按默认出口执行」 |
  | **键不存在** | 字段上线前的**旧行** | 「未知（旧记录）」—— 不给用户一个假结论 |

- **最近请求表新增「区域」列**：直接来自 usage 行的 `realm` —— 反映的是**每次对话**的真实出口，不是面板自身请求；
- **带时间**：「最近一次对话 12:03 走了国内版 · 未换区」—— 持久化字段才有的信息量。

**🐛 问题修复**
- **面板数据源从响应头迁到 usage 行**：v1.3.6 冻结的契约（`X-Qoder-Realm` 只在 chat 类请求出现）让面板**拿不到**那个头（面板发的都是管理端点）→ 改为读 usage 行，**绕开长连接状态泄漏那一整类问题**，而且**历史可查**；
- **反向保险**：新增「`dashboard.html` 全文不得再读 `X-Qoder-Realm`」的断言（**检查目标是被守护的文件，不是测试自己**），并用**金丝雀自证**（注入一行读取 → 必红）证明它不是摆设 —— 两次独立跑过；
- **两条「片段依赖自检」**：测试用的 eval 片段必须自带 `RECENT_SNAPSHOT` 声明，否则**当场红并说明原因**（而不是崩溃成一个读起来像产品坏了的 ReferenceError）；并断言该声明在 `dashboard.html` 里**顶层唯一、引用都在函数体内** —— 把「浏览器里会不会炸」变成可执行断言。

**🎨 体验优化**：见 ① 的三条。

**⚠️ 其他变更**
- **口径收紧两处**：最近一次对话取「**最近一条非 error 行**」（失败行可能没有真实出账区，拿它当结论是错的）；旧行显示「**未知（旧记录）**」而不是「没换区」；
- **流程：报告「全绿」时附两枚指纹**（被测文件 md5 + 套件副本 md5）—— 本轮一次误报（**过期副本**被读成产品故障）就是靠这条把范围锁死的；复核方也贴自己跑的那份，**先对齐再谈结论**。

**验证**
- 双入口全绿：`python tests/run_all.py` → **28 suites / 1005 checks / 0 failed**（前端六套件 **105** = 9/19/19/15/12/31）；`python _test_qoder.py` → **626 passed / 0 failed**；
- **金丝雀两次独立跑过**（作者一次、复核者一次）：注入读取 → 反向保险必红；真文件 → 全绿。

## v1.3.6

**✨ 新增功能**

**① 双区自动路由（#26）** —— 不用再手动切面板：
- **模型独占时自动试另一区**：某模型只在一个区有的（7 个 —— `gm51model` / `q37fmodel` 仅国内，`cmodel` / `efficient` / `performance` / `smodel` / `ultimate` 仅国际）会自动改用那个区；
- **无可用账号时回退另一区**；
- **面板三态开关**：自动 / 锁定国内 / 锁定国际（「自动」由 `QD_AUTO_REALM` 控制、改它需重启；锁定走既有的区域切换）；
- **本次实际出口可见**：换区时响应带 `X-Qoder-Realm` 与原因 `X-Qoder-Realm-Reason: model_missing | no_account`，面板据此显示「这次实际走了哪个区、为什么」；
- **`/v1/models?merge_realms=1`**：两区并集视图（含区域标注与 `realm_exclusive`），**opt-in** —— 默认仍是单区视图。

**边界与原则（重要）**
- **显式优先**：`?realm=` / Key 绑定 / `X-Realm` **永远优先于自动路由**，自动只在用户没指定时兜底；
- **判得出才动**：只在「模型独占」或「当前区无可用账号」时换区，判不出就维持现状；
- **MiniMax 那两个是数据驱动排除**（同 key 两区型号不同即不猜），**不是硬编码黑名单** —— 未来同类模型自动覆盖；
- **跨区会断会话亲和** —— 这是**接受的**：那两种情形下当前区本来就没有可用的亲和目标，解绑的是一条已失效绑定；
- **不违反限额护栏**：护栏是账号级的，换区即换到另一个账号，它自己的阈值照常生效。

**🐛 问题修复**
- **路由规则叠加时的优先级错误**（本轮自查发现）：账号回退逻辑只看账号数，会把「**模型规则已经推走的区**」按账号**换回原区** —— 全局在 cn、请求仅 intl 有的模型、而 intl 恰好没号时，请求会被送回 cn，打到「没有这个模型」的上游，**用户看到的是语义错误而不是「没号」**。已修：**模型归属优先于账号可用性**。

**🎨 体验优化**
- **看板：自动换区与原因可见**（三态开关 + 「本次实际出口」），并严守一条原则：**只在真的抓到响应头时才说「自动换区」**；没抓到显示「未观察到换区」；有头但与默认出口一致时显示「（与默认出口一致）」—— **不给假指示**。

**⚠️ 其他变更**
- **`tests/_tools/` 进仓**：`dash_extract.js`（锚点缺失即抛 + 按函数做纯度检查）、`dep_scan.py`（迁移依赖扫描）+ 两个自测 —— 后来人可直接复用这些判断；
- **`tests/README.md` 新增「基线数字（提交前核对用）」**：28 suites / 989 checks / 前端 89 / legacy 630，附提交前三步 —— 其中一条是「**用 `git add -A`，不要只用具体文件名**」（新增套件会被静默漏掉，表现是**全绿但总数变小**）。

**验证**
- **双入口全绿**：`python tests/run_all.py` → **28 suites / 989 checks / 0 failed**；`python _test_qoder.py` → **630 checks / 626 passed / 0 failed**；
- **自动路由逐条实测**：7 个独占模型 **7/7** 正确、双区都有的 12 个 **12/12 保持当前区**、显式优先级**六条全对**、换区可见（日志 + 响应头）、并集 **19 条**去重正确；
- **能红证据**：改动前那 7 个模型在当前区不可用（输出已留存）。

## v1.3.5

**✨ 新增功能**
- **Linux / macOS 启动脚本**（社区贡献 @jk-uzi）：`start-qoder-proxy.sh` / `start-qoder-proxy-lan.sh` / `allow-firewall.sh`，与 Windows 的 `.bat` 一一对应；运行时探测链相同、要求 Python 3.9+、用 `exec` 让终端生命周期等于进程生命周期；`allow-firewall.sh` 检测 ufw/firewalld/nftables/iptables、用 `-C` 保持幂等并打印回滚命令。

**🐛 问题修复**
- **Anthropic 入口的 thinking 档位被静默丢弃**（@jk-uzi）：`/v1/messages` 收下 `thinking.budget_tokens` 却没有把档位表传下去 → 每个请求都静默跑默认档位。**这是我们在接线时漏掉的一步**（桥接设计文档里写过）。已补 `catalog_meta_for()` 并把 meta 传进转换层。
- **非对象上游帧会让流崩溃**（@jk-uzi）：信封 `body` 可能是数组或标量，三处直接 `.get()` 就抛 `AttributeError` → 流式中途崩、聚合整单失败。现在按类型分流（放行 / 丢弃 / 不动 inner）。
- **双区账号池下冷却判断用错口径**（#29 @ismodo）：冷却函数问「全局有几个账号」，选号循环问「本区域有几个可用」——双区池里两数不等，本区域唯一的账号被当成多号池，于是上游请求的 30/60s 冷却原样生效、超出等待预算，**一次本可 3 秒自愈的排队变成中断**。两处现在共用同一个 helper；等待逻辑改为**收集候选取最小值**（不再因某个号的长频控而放弃其它号的短冷却）。
- **Windows 下网关会因系统代理开关而全挂**（#27 @mengmengma0104）：`urllib` 在 Windows 会**回退读 WinINET 注册表**，且默认 opener 在**首次请求时固化**代理配置 → 代理工具关闭后网关仍去连已无人监听的端口，直到重启。现在自建 opener、**只从环境变量取代理**：设了 `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY` 就走它，没设就直连（不回退注册表），且**每请求现读、无固化**。
- **intl 账号账本被写全 0 / 活动文案误报 / `lastCheckin` 恒空**（#25 @xiarunjie）：
  - 账本写入此前**无任何合理性守卫** —— 一个退化响应（exceeded + 两个 quota 全 0 + CN 的永不过期占位）会把健康的 399 覆盖成 0，**而且不记录原始响应**，事后无法回溯。现在：保守判定（**真·余额归零照常覆盖**）、拒绝覆盖时**落盘截断的原始摘要**、调度器**排除本轮凭证失效的账号**（不再用失效 token 打上游）；
  - 看板与一键签到把**已领取**的奖励报成「暂无可领取」—— 因为过滤发生在归类之前，而 intl 那条活动的 `actionType` 不是 `CLAIM_BENEFIT`。现在**先归类再决定是否参与领取**；`lastCheckin` 也改成**已领取时同样盖章**（此前只在「我们自己去领」时盖章 → `can_checkin` 恒真 → 调度器每轮重走同一个号）。
- **批量刷新路由对账号集合没有守卫**：`/accounts/credits`（GET/POST）、`/accounts/refresh` 会**对禁用号与坏凭证号也发请求** → 它们的账本被反复零化。现在统一走 `_refreshable_accounts()`，被跳过的号以 `{uid, reason}` 出现在响应的 `skipped` 里（**不静默少项**）。

**🎨 体验优化**
- **看板区分「未知」与「0」**：此前 `energy || 0` 让 `None` 与 `0` 塌成同一个显示 —— **这正是 #25 报告者被误导的界面症结**。现在三态分明（正常数字 / **未知**（warn 色 + 原因）/ 真 0）；批量视图的合计会标注「**已排除 N 个未知账号**」并列出是哪些。

**⚠️ 其他变更**
- ⚠️ **一条语义变化（#29 的连带）**：频控窗口 **≤10s** 时，网关会**等待并重试**，客户端收到的是**延迟后的成功**而不是 429。这是自愈（比中断好），但与旧行为不同；**>10s 的长频控不受影响**，仍返回 429。
- **测试**：编排总数到 **974 条**；`tests/README.md` 增补「**桩的保真度**」纪律（元素桩照 DOM 语义做、兄弟函数显式注入空桩、锚点缺失即抛、按函数而非区间做纯度检查）。

**验证**
- **双入口全绿**：`python tests/run_all.py` → **27 suites / 974 checks / 0 failed**；`python _test_qoder.py` → **630 checks / 626 passed / 0 failed**；
- **每条修复都带能红证据**：冷却四组数字（30→3 / wait 0→3 / 长窗保持 0 / 多号坑 0→3）· 账本污染前后（`ZEROED: ['u-disabled','u-no-cred']` → `[]`，且断言 `calls == 0`）· 文案矩阵三档一致 · **CN 对照组逐字 SAME（8/8）** · 非对象帧的 8 条 FAIL 自证；
- **一条流程留痕**：`VIEW_DETAILS` 该不该「算已领」出现过新旧断言冲突，最终判定**不该把显示与盖章分开**（分开会重现 `lastCheckin` 恒空那条链），并把原断言拆成 E3a/E3b 各自钉住新语义与「防过度」的界。

## v1.3.4

> ⚠️ **如果你在用 `1.3.2` 或 `1.3.3` 的预构建镜像，请升级到本版** —— 那两个镜像里**少了一个模块文件**，容器会**启动即崩**（`ModuleNotFoundError: No module named 'qoder_anthropic'`）。本版已修复，并加了 CI 冒烟防止再犯。

**🐛 问题修复**
- **镜像漏 COPY `qoder_anthropic.py`**（issue #23，报告者 loki0411）：`qoder_proxy.py` 从 1.3.2 起 `import qoder_anthropic`，但 `Dockerfile` 的 COPY 清单是逐个文件枚举的，**新增模块时漏了它** → 镜像里没有这个文件 → 容器启动即 `ModuleNotFoundError`、无限重启。
  - 报告者的排查很完整（镜像内 `find` 无该文件、镜像内 `qoder_proxy.py` 的 sha256 与 tag 一致、对照 1.3.1/1.2.19 自洽可启动），我们照此复现并修复；
  - **为什么我们的测试没抓到**：所有测试都跑在**源码树**上，而这是**镜像内容**问题；CI 的构建步骤**不会因为漏 COPY 而失败**（构建本身成功）。**四个发布的验证里，我们一次都没真正运行过容器** —— 这是流程缺口，不是测试缺口；
  - 受影响范围：**`1.3.2` 与 `1.3.3` 的预构建镜像**（`latest` 在 v1.3.4 发布前指向 1.3.3，因此也受影响）。**源码运行不受影响**；
  - 临时绕过（报告者提供，仍可用）：compose 里挂只读文件 `- ./qoder_anthropic.py:/app/qoder_anthropic.py:ro`；升级到本版后可移除。

**✨ 新增功能**
- **CI 冒烟（防复发）**：构建之后新增一步，用**本地单架构镜像**实际跑三件事 ——① **按源码里的 `import qoder_*` 反查每个模块文件是否真的在镜像里**（这次就是这么漏的，将来任何新增文件忘 COPY 都会立刻红）；② `import qoder_proxy, qoder_anthropic` 确实可导入；③ 起容器并**等 `/ping` 回应**（60 秒超时，失败打印容器日志）。
  > 一句话教训：**构建成功不等于镜像能跑**。

**⚠️ 其他变更**
- README 安装段补了**镜像与版本直达入口**（GHCR 包页面 + Releases 链接、可用 tag 形状、双架构说明、以及「仓库侧栏未必列出容器包」的提示）。

**验证**
- 双入口测试全绿：`tests/run_all.py` → 19 suites / 881 checks / 0 failed；`_test_qoder.py` → 630 checks / 0 failed；
- **镜像层验证由新增的 CI 冒烟承担**：本版 CI 里会执行上面三步（其中第 ① 步在修复前必然失败 —— 因为 `qoder_anthropic.py` 不在镜像里）。

## v1.3.3

**✨ 新增功能**

**① 前端测试破题**（工作包点名的第五类空白）：`tests/` 新增 **4 个前端断言套件（62 条）**，把前面几轮的前端证据**从会话里搬进仓库**：
- **`effort_parity`（9 条）** —— **逐行审计列与汇总视图的四类计数一致性**。这是这批里最有价值的一条：同一份数据两处实现，口径一旦漂移，运维就会看到**互相矛盾的数**（与我们 `sum(by_key)==sum(by_account)` 是同一类问题）；
- **`limits_auth`（19 条）** —— **403/401 分流**（把 403 当鉴权失效会让用户白白掉登录、真原因被吞 —— 这是修过的真 bug 的回归网）+ 限额保存载荷形状（空串=继承 / 0=关闭 原样传递）；
- **`effort`（19 条）** —— 档位三态渲染 + Key 列四态；
- **`log_and_badge`（15 条）** —— 日志**增量追加**的行为断言（追加而非重建）+ 护栏四态徽标。
- 做法：**自读 `dashboard.html` + 抽 script 块 + 内联元素桩**，**没有引入 jsdom**（守住零依赖红线）；配两个工具：**`tests/_tools/dash_extract.js`**（锚点缺失即抛 —— 防「切片拿到空串、断言照样通过」这种**假绿**）与 **`dep_scan.py`**（迁移依赖扫描）。

**② 按 Key 的档位分布**（矩阵下方）：一眼看出**哪个 Key 受影响最多**（多少请求被挪档、多少被丢弃）。这是把 P1-3 的审计口径从「逐行可见」升级成「按 Key 汇总可见」；**零额外请求**（复用已拉回的最近请求行）；口径行明写「基于最近请求页样本，**不是全量聚合、数字不可相减**」。

**🐛 问题修复**：Key 列 tooltip 不再出现后端桶名的字面量 —— 界面（含 tooltip）均不泄露实现细节。

**🎨 体验优化**：见 ② 的汇总视图。

**⚠️ 其他变更**
- **测试迁移策略调整（有数据支撑，不是妥协）**：写了一个依赖扫描工具（提取每段定义的全局名 → grep 后续段引用），跑完 **55 个段**后 `--leaf-only` **没有任何输出 —— 不存在纯叶子段**（段间共享显式全局如 `time` / `base64`，也有隐式的、供后续段继承的绑定）。
  **结论：逐段迁移在这个文件结构下，边际成本已高于收益。** 因此调整为：**legacy 作终态**（它是**在用的测试文件**、630 条断言、持续全绿，不是技术债）、**新测试一律走新套件**、迁移只做低风险部分 —— 让它**自然冻结**，而不是强行拆完。
- **迁移期三条纪律**（三次回滚换来的）：① **迁移与修 bug 不混在同一次编辑**；② **共享头部显式抽出**（别让每个套件自己抄 import）；③ **先做依赖扫描**（只有叶子段适合独立成套件）。

**验证**
- **双入口全绿**：`python tests/run_all.py` → **19 suites / 881 checks / 0 failed**；`python _test_qoder.py` → **630 checks / 626 passed / 0 failed / 4 skipped**；
- **4 条「改坏 → 必红」自证**：parity（9 FAIL）· auth（7 FAIL）· log（10 FAIL）· effort（EXIT=1）—— 每条新套件都证明了自己**真的在守**，而不是「看起来绿」；
- **一条工具经验**：改坏点里含**非 ASCII** 时 PowerShell 的转义会坑人（试两次没命中），**改用 Python 按行号替换**一次成功 —— 已记进团队纪律。

## v1.3.2

**✨ 新增功能**

**① Anthropic 原生入口 `/v1/messages`** —— Claude Code 这类 Anthropic 客户端可直接接入：
- 新增独立转换模块 `qoder_anthropic.py`（**零依赖纯标准库**）：入站映射（14 个字段）、出站（非流式 + 流式事件流）、usage 守恒映射、effort 断点映射（逐模型 clamp + 单调不降）；
- **接线的三条要害**（都配了断言）：① **`x-api-key` 头**（Anthropic 客户端不用 Bearer，漏了会**全量 401**）；② **形态决策单点**（结构化与否只判一次；两处各判会出现「桥接层以为结构化、flatten 走文本化」的错配）；③ **守卫链逐字复用**：`sse_with_heartbeat(recover_leaked_tool_calls(iter_inner_sse(...), allowed_names=...))`，且 `allowed_names` 必须取**转换后的** chat 请求（用 Anthropic 原始 payload 会取不到工具名 → 回读守卫失效）；
- **泄漏回归（新入口）**：#8 / #9 / #11 三条历史样本走新入口 → **守卫仍然生效**（有日志实证），#8 还能正确还原成 `tool_use` 块。**新入口没有变成第三个绕过守卫的门**；
- **三条已知差异**（已写进 README 已知限制）：**流式不做流内信封重开**（`message_start` 已发出、重开等于两个 start，协议违规；改为发明确的 `event: error` 终态事件而非静默关流；非流式仍完整重试）· **默认不发 thinking block**（Anthropic 要求 `signature`，我们生成不了也**不伪造**；`QD_ANTHROPIC_THINKING=empty` 为实验开关）· **入站 thinking 一律丢弃**。

**② 模型闸门**：全局 `banned_models` + 每个 API Key 的 `models` 白名单（fnmatch 通配）。
- **统一落点 `_model_precheck`、三个入口共用**（chat / responses / anthropic）—— **不是三份实现**，天然没有「某个入口绕过」的缺口（已用断言覆盖 `/v1/messages`）；
- **拒绝发生在本地**：出站层与网络层**两层计数均为 0**；并含**对照组**（白名单内放行）与**默认不变**（空配置放行）—— 防「一律拒绝也绿」；
- 同理，`_clean_key_entry` 是白名单重建，**`models` 必须显式保留**（否则每次保存被静默丢弃 —— 与早前 limits 同型的坑，已断言钉住）。

**③ 会话亲和长度上限**：`QD_AFFINITY_MAX_MSGS`（**默认 0 = 关闭**）。⚠️ **阈值没有照抄参考项目** —— 那张断连率表来自单一实例、333 个请求，上游也不同；等我们自己的数据再定。

**🐛 问题修复**：无

**🎨 体验优化**：Anthropic 入口的三条已知差异写进 README（读者需要知道边界在哪）。

**⚠️ 其他变更**
- **`tool_choice="none"` 形态对齐**：从「连 `tools` 一起删」改为「**保留 `tools` + 写回 none**」。上游 body 对比确认**只影响 `tools` 一个字段**（其余 7 个非随机顶层键全同）；开关 `QD_TOOLCHOICE_NONE_DROPS_TOOLS=1` 可一键回退。
  - ⚠️ **一条反例（如实登记）**：参考项目自己的注释说「**其上游并不真正遵守 none**」（保留 tools 后仍回 `tool_calls`），**与我们工作包对 qoder 的实测相反**。我们按 qoder 的实测默认新行为，但**留了回退开关** —— 若观察到 `none` 下仍回 `tool_calls`，设它即回旧行为。
- **出站收口**（P1-2 第一步）：把带凭证的直连收进统一出站函数（不变量基线 **3 → 2**，且**中间态红过**，证明断言在守）；**并判定「按账号出站代理」不做** —— 账号全部由用户本机导入（出口天然相同）、全局代理已**零代码支持**（`HTTPS_PROXY`，README 已补）、无任何「上游按 IP 风控」信号。若将来真撞上，最小方案约 25 行（已留档）。
- **测试工程化**：`tests/` 编排现 **15 个套件 / 819 断言**；从旧单体文件**已迁出 4 段**（每批双入口对比、账目精确）；迁移期两条纪律已写进 `tests/README.md`（路径一律基于 `__file__`；每套件自己的临时目录、不写仓库内共享路径）。

**验证**
- **双入口全绿**：`python tests/run_all.py` → **15 suites / 819 checks / 0 failed**；`python _test_qoder.py` → **626 checks / 0 failed / 4 skipped**；
- **本轮新增能红证据 3 组**：Anthropic 模块（改前 `ModuleNotFoundError`）· 接线（**4 红 → 16 绿**）· 模型闸门（**4 红 → 16 绿**，明细显示`未实现时请求放行到出站口`）；
- **旁路核查**：确认模型闸门三入口共用同一判定，**不存在「chat 被拦、messages 绕过」**；
- **一处自我更正也已登记**：某次 `run_all` 出现过 1 failed，追查为**撞上并发编辑的中间态**（不是回归）；重跑 0 failed。这也提醒：**并行开发期的瞬时红要先排除中间态再当回归**。

## v1.3.1

**✨ 新增功能**

**① 测试工程化** —— `tests/` 并发编排上线：
- `tests/run_all.py`：**并发**、每套件独立日志、失败打 tail 25 行、强制 UTF-8、无匹配套件返回 2（防「0 passed / 0 failed → exit 0」这种假绿）；
- **11 个套件 / 747 条断言**，含工作包点名的三类空白：**代理不变量**（参数穿透式断言 + 基线登记）、**keep-alive 早拒**（裸 socket）、**前端 DOM 断言**（首个 `.js` 套件，自读 dashboard.html + 内联元素桩）；
- **兼容入口保留**：`python _test_qoder.py` 照旧可用；套件正逐段从它迁出（本轮已迁 2 段，legacy 仍承载其余 —— **迁移期间总数只增不减**）；
- ⚠️ **一处更正**：工作包让照抄的 wb `tests/_dom_stub.js` **并不存在**（递归查 0 命中）；它的真实做法就是「每个套件自读 dashboard.html + 内联手写约 15 行元素桩」，我们按后者实现，没有引入 jsdom（保住零依赖）。

**② 账号退避 / 熔断 / 降权三族**：
- `note_soft_rate`（软限流**指数退避**：600 → 1200 → 2400，封顶 7200）· `note_failure`（硬错误**熔断**）· `note_unknown_failure`（未知错误**降权**）· `note_success`（统一复位入口）；
- **接线 6 处** —— 这正是教训所在：三族在接上调用点之前**是死代码**（有断言证明接线前 6 处计数全 0）；
- **防误接（反例断言）**：直连 401/403 · **信封 429（10605 排队，接了会让一次排队冷掉全账号）** · 内容审核 · **402 余额不足** · 单号池 → 三族**零调用**；
- **单号池不接**（`total>1` 条件）：单号池接熔断/长退避 = **变相打死账号**；
- 运维恢复：三族状态是**内存态**（不落盘）→ 重启即清 / 等窗口自动过（30 分钟起、封顶 6 小时）；面板的「重新启用」走 `clear_error`，**有意不清熔断**。

**③ 用量字段与聚合**：
- usage 行补 `reasoning_effort` / `reasoning_effort_requested` / `key_id`（全脱敏）；**三态可辨**：同值 = 原样透传 · 不同 = 被挪档 · **空串 = 请求了但未下发**；
- 新增 **`GET /usage/by-key`**（与 `/usage/by-account` **逐字对称**；`sum(by_key) == sum(by_account)` 不变量已断言；缺 `key_id` 的行归 `(no-key)` 桶、**不丢弃**）；
- 面板：最近请求表加 **Key** 与 **档位** 两列（档位三态一眼可辨）· 指标页新增**按 API Key 用量透视**（Key / 请求数 / 输入 / 输出（含思考）/ 缓存 / 总 token / 占比 / 积分 / 模型前 5，按 token 降序）。

**🐛 问题修复**
- **临期积分分派：管道就绪、当前不触发** —— 实测上游 `/quota/usage` **不返回 per-package 到期字段**（只有顶层 `expiresAt`，且它可能是「按月重置」而非「到期作废」）。处置：**防御性透传**（上游一旦补字段即自动生效、无需改码）+ **刻意不把顶层 `expiresAt` 贴到包级** —— 硬贴会让「按月重置」的额度被**优先消耗**（反向浪费）。

**🎨 体验优化**：见 ③ 的两处 UI（档位三态 + 按 Key 透视）。

**⚠️ 其他变更**
- **测试方法论（写入团队配方）**：parity 类断言**只能用 `requests` / token / `credit`**（`models` 是**前 5 名截断**的展示字段，拿它求和必假失败）；error 行**两侧都跳过**；窗口按 `row["at"]`（epoch）过滤、**完全不看 `iso`**，且**缺 `at` 的行被当 0**（夹具必须给 `at`）；窗口边界是**闭区间**；**桶数不等是正常语义**（一个 Key 可打多个账号）。
- **验证配方补充**：任何「逐字节一致」类对照**必须用固定夹具或同一份快照** —— 真实日志仍在被追加（`analytics` 统计 error 行、 `by_account` 跳过），否则会把「日志在长」误判成「改动引入差异」。

**验证**
- **双入口全绿**：`python tests/run_all.py` → **11 suites / 747 checks / 0 failed**；`python _test_qoder.py` → **658 checks / 0 failed / 4 skipped**；
- **每个新套件都有「改坏 → 必红」自证**（本轮 6 条：护栏等号边界 · 代理绕过点 · 前端配色 · 三族接线计数 · parity 口径 · dashboard 渲染）；
- **三族接线**：接线前 **6 红**（计数全 0，正是 6 个待接点）→ 接线后 **13 绿**；
- **parity 修绿**的过程本身也是个证据：期望值先按实测口径**重算**（无窗口 645 / day 145），而不是改实现去迁就断言。

## v1.3.0

> **自 v1.2.12 以来的主要变更概览**：机器身份落盘缓存 + 种子机制修正（容器重建不再换设备）、VM 抖动多数表决（不把身份固定在少数派）、工具历史结构化直传、只读积分路由 `GET /credits/summary`、usage credit 统计修复 —— 以及本版两项：**事实性限额护栏**与**用量区间 / 时间序列**。

**✨ 新增功能**

**① 事实性限额护栏** —— 账号级预算闸门：**用尽自动换号**，而不是等上游返 402/429。
- 四条护栏：**余额地板**（`reserve_credits`）· **当日 token**（`daily_token_limit`）· **当日 credit**（`daily_credit_limit`）· **单模型当日 token**（`model_daily_token_limit`）；
- **三层作用域**（global + realm 两级）：**留空 = 继承**、**显式 0 = 关闭**；
- **后台余额刷新**：每 tick 最多刷一个最陈旧账号（30min tick + 12h TTL），**惰性启动**（阈值全 0 时线程根本不存在，零副作用）；
- **计数来自本地 usage 折叠** —— 不新增任何上游调用；
- **402 / 额度停放**：停到 **UTC+8 每日 10:00**（qoder 自己的重置窗口，非通用 4am），**仅余额恢复**可提前解封；
- **临期积分优先分派**：7 天内到期的额度优先消耗（smooth weighted round-robin），**默认窗口 0（关）**。
  - ⚠️ **当前状态（重要）**：上游额度接口**不返回 per-package 到期字段**（实测只有 `total/used/remaining/percentage/unit`，到期信息只有顶层 `expiresAt`）→ **即使开启窗口也不会生效**；
  - 我们做了**防御性透传**（上游一旦补字段即自动生效，无需改码），并**刻意不把顶层 `expiresAt` 贴到包级** —— 它可能是「**按月重置**」而不是「到期作废」，硬贴会让这类额度被**优先消耗**（反向浪费）；
- **面板**：设置页限额表格（带**量纲提示**）+ 账号行**四态徽标**（未启用 / 未知 / 已暂停 / 正常）。

**② 用量区间与时间序列**：`/usage/analytics` · `/usage/by-account` · `/usage/perf` 支持 `?range=day|week|month|all|custom&since=&until=`；新增 **`/usage/timeseries`**；`realm=all` 现在是真正的通配符（此前 `/usage/perf` 采样恒为 0）。

**🐛 问题修复**
- **限额设置此前不会生效**（接线缺口）：**没有任何生产代码**把 limits 推给账号池 —— 配置写进 `settings.json` 就停住了，阈值恒 0、守卫永不触发。现在**启动时**与**设置保存后**各推一次（free 名单同批推送，避免空集拦住全部模型）；
- **后台刷新线程此前会静默死掉**：`AccountPool.log` 默认实现只接受一个位置参数，而刷新线程传了 `level=` → 一 tick 就 `TypeError` 退出，整条刷新链路失效；
- 设置页改阈值现在会打印状态翻转日志（此前只有其它路径打）。

**🎨 体验优化**：面板两处（限额表格的量纲提示、账号护栏四态徽标）—— 见上 ① 末条。

**⚠️ 其他变更**
- ⚠️ **量纲说明（重要）**：qoder 的 credits 是**积分点数**（实测账号区间 **0~1100**），**不是 token 量级** —— 照 token 量级填阈值会**永远拦不到任何账号**；
- **默认全部关闭（0）** —— 不给任何非零默认值；
- 参考材料：本仓 `.team/_gap/`（双向差距分析 6 份文档 + 实测证据）。

**验证**
- 全量 **693 checks / 0 failed**；
- **端到端（真实链路，非桩）**：配置 → `quota_enabled=True` → **后台线程启动** → 守卫命中 → `pick` 返回 None → **网络层调用计数 0**（确实没发上游）→ 复位后账号恢复；
- 配置层 A1-A5 · 守卫层 B1-B6 · 汇合点 C1-C3 · 折叠 D2-D4 · 后台刷新 E1-E5（含惰性启动）· 派发偏好 F1-F6（含「默认关时与改动前等价」「永不过期占位不参与权重」两条边界）· 停放时钟 G1-G2 **全部通过**；
- **一条流程留痕**：这条端到端判据前五轮不闭环，根因是**实现侧接线缺失 + 测试侧判据位置错误**各占一半 —— 判据应拦在**网络层**（是否真的出网），而不是「`open_upstream` 是否被调用」（网关正常路径本来就会进它、由它内部选号）。

## v1.2.19

**✨ 新增功能**：无

**🐛 问题修复**
- **usage 记录（与面板）的 credit 统计恒为 0**（issue #22，by @jianjiuss）：`_extract_usage()` 读的是**单数** `usage.get("credit")`，而上游给的是**复数 `credits`**（附 `original_credits` / `billable`）—— `usage.get("credit")` 恒为 None，于是**所有** usage 记录都记成 0（报告者实测 426 条流式 + 16 条非流式全部为 0）。
  - 现在**复数 `credits` 优先、保留单数兜底**（防其它区域字段差异）；
  - 顺带记录 **`original_credits`**（**参与聚合** —— 折扣时段对账要用）与 **`billable`**（仅写进行、**不进聚合** —— 布尔求和没有意义）。

**🎨 体验优化**：无

**⚠️ 其他变更**
- **历史数据不可回填**：修复只影响此后写入的行；此前记录里的那个 0 是**永久性的**（当时的原始 `credits` 从未落盘）。我们不会在日志或文档里暗示它可恢复。

**验证**
- 全量 **657 checks / 0 failed**（[39] 段 10 条）；
- **修复前的红色证据已留存**（落盘前抓到的原始输出：复数 `credits=97.96` → `credit: 0`；两者都有时旧实现取单数）—— 没有它，这个回归网就是空转；
- **变异自证 4 条**：改回只读单数 → 必红；单数优先 → 必红；抽掉 `original_credits` → 必红；把 `billable` 塞进聚合表 → 必红；
- **面板不受影响**（已 grep 确证：它读的是账号对象的 `credits.remain`、模型元数据的 `credits`、签到侧的 `earned_credit`，**没有一处**读 usage 行的 `credit`）；
- 未覆盖（如实）：`credits` 为字符串或 `None` 的形态（`or` 链会把 `"0"` 当假值，属理论边界）。

## v1.2.18

**✨ 新增功能**
- **`GET /credits/summary`：API-key 级鉴权的只读积分汇总**（issue #21）——给下游集成（如网关池面板）展示各账号池余额用。
  - 鉴权走 `_key_ok()`（**API Key 或面板会话均可**，未设 Key 时放行），**刻意不放进 `_is_panel_route()`** —— 否则就会要求 `X-Panel-Token`，与用途相反；
  - **只返回本地已登记的 `credits`，绝不触发任何上游查询** —— 这是本 issue 的核心约束（对照 `GET /accounts/credits`：那个会逐号 `fetch_credits()` 强刷，适合作者手动点、不适合机器轮询）；
  - 返回 `{ by_realm, totals, accounts[], note }`，每行含 `{uid, nickname, realm, credits_remain, credits_used, credits_size, registered_at}`；
  - **新鲜度语义写在字段名与 `note` 里**：返回的是「上次登记时的值」（签到写权威值 + 每次消耗回写），**不是实时余额**；
  - 未登记过积分的账号**照常列出**但 `credits_*` / `registered_at` 为 `null` 且**不计入汇总**；空池 / `POOL=None` 返回空结构而非报错。

**🐛 问题修复**：无

**🎨 体验优化**：无

**⚠️ 其他变更**
- README 的接口一览与说明补上该路由（含「为什么不能拿 `/accounts/credits` 轮询」的对照）。

**验证**
- 全量 **647 checks / 0 failed**（[38] 段 12 条）；
- **命门**：`fetch_credits()` 桩计数 **= 0** —— 且这次用**真实 handler** 验证（`ThreadingHTTPServer` + 后台线程 + `urllib` 真请求，鉴权 / 路由匹配 / JSON 写出全走真代码路径）；
- **能红证据**：把实现改成「先 `fetch_credits()` 再返回」→ 命门断言必红（变异下桩被调 **3** 次 = 3 个账号各一次）；
- **鉴权矩阵**：有效 key 200 / 无效 key 401 / 无凭据 401 / 面板会话 200 / 未设 key 放行 200（**全部 0 次 fetch**）；
- **边界**：空池 / `POOL=None` / 无 credits 账号 / 部分 credits 全绿。

## v1.2.17

**✨ 新增功能**：无

**🐛 问题修复**
- **首次落盘可能把身份固定在少数派分支上**（issue #18 的最新发现）：组件的 **VM 判定会抖** —— `machineToken` 恒定，但 `machineType` / `machineCode` / `vmInfo`（`brand` / `vmTypeCode`）有概率落到另一个分支（`KVM/13` ↔ `Docker/50`）。而缓存存的是整个三元组，**首次落盘掷到哪边就永远固定哪边** —— 而 `vmTypeCode` 正是喂风险画像的字段。
  - **本机实测抖动率 10.29%（18/175）**，比报告者的 1.56%（4/257）**高 6.6 倍**（差异原因如实标注为**线索**：WSL2 后端 vs 原生内核的信息读取路径，未做对照实验）；
  - 现在首次写入缓存时**多数表决**：**先 3 次**（全一致直接采纳，约 73% 情形、成本 ~11s）→ **有分歧补投到 5 次**（上限 5、~18.5s）→ 取多数派的**完整样本**（**绝不逐字段拼装**）；
  - 表决键刻意**排除 `vmInfo.percentage`**（连续噪声会让表决失效）；平票 / 全分歧取首样本（确定性）；
  - **新增日志**（报告者指出这是排查盲区）：`identity vote: 3/3 KVM (vmTypeCode=13)` / `… extended to 5 -> 4/5 KVM (1 minority Docker)` / `… no majority (…); using first sample`；
  - 新增开关 `QD_MACHINE_IDENTITY_VOTE=0`（跳过表决、只调一次 —— 给「要最快首启、不在乎这点概率」的人）。

**🎨 体验优化**：无

**⚠️ 其他变更**
- 更正一处归因（同样来自本轮实测）：**`vmInfo.brand` 不固定与镜像无关**，真实原因是同一个容器内就会在这两个分支之间抖。

**验证**
- 全量 **635 checks / 0 failed**（[35] 段 21 条：表决 9 条 + 缓存 12 条）；
- 抖动复现：连续 160 次 **9.38%**、间隔 60s 15 次 **20%**（合计 175 次 **10.29%**），少数派出现位置**非周期** → 支持「每次调用独立按概率抖动」；
- **两套独立装置**（实现的探针 vs 测试的断言）逐项交叉验证一致；
- **未覆盖（如实）**：补投的端到端延迟（当前为组件耗时推算）、日志文案断言、多进程并发读写同一缓存文件。

## v1.2.16

**✨ 新增功能**：无

**🐛 问题修复**
- **服务端「不认身份」时的自愈此前是空转**（据 issue #18 的实测）：自愈会清掉缓存并重新调用官方组件，但**同一容器内组件返回的是同一个身份** —— 它拿到的正是刚才那个被拒的身份，账号会一直卡到容器重建为止。
  - 根因：组件身份由它自己写的**随机种子文件** `$HOME/.config/.locale_cfg`（350 B）决定，**不随时间轮换**（实测：同一容器内 69 分钟 24 次采样、以及我们自己的 10 分钟 11 次采样，身份均完全不变）；
  - 现在：自愈会**先清种子再取身份**（实测由此必得新身份）。
- **`QD_MACHINE_IDENTITY_RESET=1` 此前换不出身份**：它只清了缓存、种子没动 → 组件会给出**完全相同**的身份。现在也一并清种子。
- **`QD_MACHINE_IDENTITY_CACHE_TTL` 此前是空开关**：到期只是重新调一次组件、拿到同一个身份。现在 TTL 到期会清种子并取新身份 —— **这个开关从此真的能轮换**。

**🎨 体验优化**：无

**⚠️ 其他变更**
- **更正两处被实测证伪的说法**（原写在 v1.2.13 的说明与文档里）：
  1. 「组件本身不落状态文件」—— **错**。它会落 `$HOME/.config/.locale_cfg`（随机种子）；
  2. 「身份会随时间轮换」—— **错**。同一容器内身份恒定，**换身份的本质是换种子**。
- 文档补上**三输入模型**：`identity = f(种子, 机器证据[DMI / CPU]，容器证据[overlay 挂载行、网卡 MAC])` —— 同时解释了「同一容器内恒定」与「重建必换」；
- 三条「会自动换身份」的路径写进文档：**自愈 / `RESET` / TTL 到期**；**其余路径一律不碰种子**（否则「重建不换」会被自己削弱）。

**验证**
- 种子机制：**删种子 → 身份必变**；保留种子 → 同一容器内恒定（我们 10 分钟 11 次 + 对方 69 分钟 24 次，双环境一致）；
- 三路径闭环（WSL 真机、真组件 + 真种子 + 真缓存）：首次**不清**种子 ✓ ／ TTL 到期**清**且身份**真的变了** ✓ ／ 缓存命中**不碰** ✓；
- 全量 **626 checks / 0 failed**。

## v1.2.15

**✨ 新增功能**
- **签到结果新增「下次可签到时间」**：`/accounts/checkin` 的每个 result 现在带 `next_available_at`（下一个「每日 10:00（UTC+8）」的 epoch 秒）与 `next_available_note`（UTC+8 固定呈现，如 `"10-05 10:00（UTC+8）"`）——**无论本次是否真的领到都会给出**，用户可据此自我核对。

**🐛 问题修复**
- **「签到成功」不再掩盖真实结果**（issue #20）：此前面板把「已领取」「暂无可领」「名额发完」一律渲染成「签到成功」，用户以为积分丢了。
  - **字段级根因**：HTTP 出口（`qoder_proxy.py`）的 result 项**只有 `msg`＝签到日志末行，既没有 `message` 也没有 `claimed`** —— 而 campaign 路径的日志末行是「当前额度余额: N」，**根本不是结论行**。前端的 `x.message` 一直是 undefined，只能写死文案。
  - 现在 `run_checkin` 与 HTTP 出口**原样带出**活动平台的结论（`message` / `claimed` / `next_available_*`，**新增而非替换**，`msg`/`logs`/`credits` 保持不变）。
- **前端按「是否真的到账」分流**：`claimed` 非空才说成功；否则如实说明原因并附下次时间。同时修掉一处字段名笔误（失败分支读 `x.msg`）。
- **配色不再骗人**：只有真的到账才用成功色，全失败用错误色，其余（已领 / 无可领 / 混合）用提醒色。
- **旧响应兜底**：若后端未给 `message`（旧 sash 兜底路径），前端会从 `logs` 里挑**结论行**（`✓/⚠/—/!` 优先），并明确**不采信余额行**。

**🎨 体验优化**
- 多账号签到：≤2 个逐条列出；>2 个改为「到账 N（名字…）· 已领/无可领 M（名字…）· 失败 K（名字…）+ 首条明细」，谁到账谁已领都点名。

**⚠️ 其他变更**
- 时区：`next_checkin_window()` 以 **UTC+8 固定**计算（容器本地时区常为 UTC，按本地渲染会与官方说明差 8 小时）；已用 `TZ=UTC` / `TZ=America/New_York` 双进程验证结果一致。

**验证**
- 全量 **623 checks / 0 failed**（新增 28 条：前端分流 16 条 + 后端窗口边界 12 条）；
- 后端窗口：**20 万个随机时刻**与纯整数参考实现逐一比对（0 mismatch）+ 8 个边界（10:00 前后 / 跨月 / 跨年 / 闰日）；
- 前端：三条路径渲染 + kind 六组合 + 时区无关三跑一致 + **能红证据**（改回写死文案则断言变红）。

## v1.2.14

**✨ 新增功能**：无

**🐛 问题修复**：无

**🎨 体验优化**
- **Docker 部署开箱更省事**（来自 PR #19，by @Tinkler-i）：`docker-compose.yml` **默认改用 GHCR 上的预构建多架构镜像**（`build: .` 保留为注释，想自己编取消注释即可）——NAS 用户升级只需 `docker compose pull`，不必在本机编译；
- **新增 healthcheck**：用**免鉴权**的 `/ping` 端点（interval 60s / timeout 5s / retries 3 / start_period 30s）；
- README 的 Docker 段落改为「**预构建镜像优先、源码构建为备选**」。

**⚠️ 其他变更**
- `docker-compose.yml` 的 `mac_address` **改为默认注释掉**：v1.2.13 的落盘缓存已经覆盖「重建 / 升级」（只要 `accounts/` 是挂载卷），固定 MAC 从「推荐」降为「**可选**」——它本身有撞 MAC 的副作用（macvlan、同宿主机多实例）。注释里写明何时才需要它，并保留「**请换成你自己生成的 MAC**」提醒与生成命令；
- 卷挂载注释补「NAS / 长期部署建议用绝对路径」。

**验证**
- `docker compose config` 语法自检通过；
- PR 作者在**真实官方镜像 + 真实 compose**（Docker 29.6.1 / arm64）下验证过 `mac_address` 与 `/ping`；
- `/ping` 的免鉴权性质我们另行确认（它本就是为探活设计的端点，不查面板密码与账号池）。

## v1.2.13

**✨ 新增功能**
- **机器身份落盘缓存（默认启用）**：把官方风控组件**首次输出的**设备身份缓存到 `accounts/machine_identity.json`，之后**优先复用**——**根治 issue #18 的「重建 / 升级容器必换设备身份」**（挂同一卷重建后身份逐字节不变）。缓存的是**官方组件自己的输出**，组件本身仍保持「不落状态文件」的官方行为。

**🐛 问题修复**
- **领取路径不再每次强制刷新身份**：原来每次签到/领福利前都会刷新设备身份，导致 (a) 每次领取换一套身份 (b)刚落盘的缓存被自己刷掉。现在改为走缓存优先链路——是否刷新交给「服务端不认时才自愈」的逻辑。
- **组件不可用时不再退化为派生假身份**：过去原生桥跑不起来（如 alpine 缺 glibc 兼容层）只能退到 `derived`；现在会**回退到落盘缓存里的真身份**（哪怕已过期）。

**🎨 体验优化**
- 缓存事件有明确日志：首次启用打印一次「正在复用缓存身份」、自愈覆盖打 WARN、缓存损坏打 WARN、缓存与组件输出不一致打 INFO —— 避免「说好不变怎么又变了」的困惑。

**⚠️ 其他变更**
- 三个开关：`QD_MACHINE_IDENTITY_CACHE`（`auto` 默认 / `off` 关闭）、`QD_MACHINE_IDENTITY_CACHE_TTL`（默认 `0`＝不过期；**设了就会按周期轮换**）、`QD_MACHINE_IDENTITY_RESET=1`（启动时清空，也可直接删文件）；
- 文档：Docker 段落与「已知限制」均写明落盘缓存为首选方案，并点明**前提是 `accounts/` 为挂载卷**、**不要跨机器共享该卷**。

**验证**
- 全量 **598 checks / 595 passed / 0 failed / 3 skipped**（新增 12 条离线断言，含两条命门：**第二次调用不触发组件**、**自愈后落盘被更新**）；
- **真实 Docker 端到端**：挂同一卷重建 3 次 → 身份**逐字节相同**（日志 `reusing cached identity for cn (rebuilds keep the same device)`）；不挂卷对照 → 三次**互不相同**（证明断言有效、非环境恒定）；
- 真实链路演练：首次 4.42s（真跑组件）→ 复用 **0.001s**，身份逐字节一致。

## v1.2.12

**✨ 新增功能**：无

**🐛 问题修复**：无

**🎨 体验优化**：无

**⚠️ 其他变更**

**文档：把「固定网卡 MAC」提为首选方案**（issue #18 作者用真实镜像 + 真实 compose 实测后建议）
- 「设备身份稳定性」提示改为**两条路并列且有序**：
  - **① 固定网卡 MAC（推荐，代价最小）**：留在默认 bridge、**保留 `ports:` 映射**——`mac_address: "02:42:ac:11:00:77"`；
  - **② `network_mode: host`（备选）**：适合宿主机端口冲突、或需要容器直接占用宿主端口时。
- 补**兜底写法**：若 service 级 `mac_address` 被拒（官方文档提到 Engine ≥ v25 可能拒绝），改用 `networks.<网络名>.mac_address`（实测 29.6.1 / 29.7.2 均接受）。
- 附**机制解释**（两输入模型 `identity = f(每容器独有的量, 容器网卡 MAC)`），并明确**两条路都只解决「重启」这一类**、重建容器仍会换。

**验证**
- 独立复现（Docker Desktop 29.7.2 / WSL2）：service 级 `mac_address` 被接受、实际 MAC 与设定一致、端口映射正常、restart 与 stop/start 后 token 均逐字节不变、重建仍会变——与作者的 29.6.1 / ARM 结果逐条一致；
- 额外确认 `mac_address` 与 `ports:` 并存无冲突。

## v1.2.11

**✨ 新增功能**：无

**🐛 问题修复**：无

**🎨 体验优化**：无

**⚠️ 其他变更**

**文档（issue #18 的补充实验：驱动量定位）**
- Docker 部署段落的「设备身份稳定性」提示现在**并列两条路**：
  1. `network_mode: host`（彻底共享宿主网络栈）；
  2. **`mac_address` 固定网卡 MAC + 保留 `ports:` 映射**——只消掉「重启就换」这一类，代价更小：
     ```yaml
     services:
       qoder-proxy:
         mac_address: "02:42:ac:11:00:77"
         ports: ["8790:8790"]
     ```
- 附上机制解释（两输入模型）：容器身份取决于「**每容器独有的量**」与「**容器网卡 MAC**」——bridge 下停止会重建 veth、MAC 随之改变，故固定 MAC 即可让 `stop/start` 后身份稳定；**重建容器仍会换**（由「每容器独有的量」决定，compose 层无法覆盖）。

**验证**
- 独立复现三项关键实验：**共享 netns 的容器身份仍不同**（排除 netns）、**不固定 MAC 时身份随 MAC 同步变化**、**固定 MAC 后两次 stop/start 逐字节不变**；
- 额外验证 **compose 的 `mac_address:` 服务键**在 restart 后同样稳定，且在 **Docker Desktop / WSL2** 上同样生效（原报告来自 fnOS / Docker 28）。

## v1.2.10

**✨ 新增功能**：无

**🐛 问题修复**：无

**🎨 体验优化**：无

**⚠️ 其他变更**

**测试**
- 新增端到端回归 `_test_leak_guard.py`（来自 PR #17，by @849506054）：把真实生产泄漏形态喂进 `recover_leaked_tool_calls()` 走**完整 SSE 管线**，再按客户端视角断言「到底看到了什么」——补上判据层覆盖不到的**管线接线层**（v1.2.7 那个重复输出正是这一层的缺陷）。含 5 个泄漏形态 + 3 条误伤用例，可在发布镜像内直接跑（纯标准库、无 fixture）。

**文档（issue #18 @Tinkler-i 的对照实验 + issue #10 的补充实测）**
- 「已知限制」的容器身份条目**更正**：默认 `bridge` 下 `stop/start`（含 `compose restart`、宿主机重启自动拉起）**也会换**；`network_mode: host` 下**不变**；**任何网络模式重建都换**（compose 层无法固定）。并标注影响面：只有活动/签到链路用到真身份，**推理链路用的是按 UID 派生的身份、不受影响**。
- Docker 部署段落补实践建议：用 `network_mode: host`（**切换前确认端口空闲**，否则容器启动失败）、避免高频重建；该方案已在 **amd64 / arm64 双架构**、真实官方镜像 + 真实 compose 下验证。
- 「已知限制」补：**Linux 实体机也可能 `isVm=true`**；`vmInfo` **只用于本地诊断、不上行**。
- 「已知限制」补 intl 每日 100 的「**下发 vs 领取**」区分：**下发**绑定设备（由官方客户端登录的那台机器决定），而**领取不校验机器头**——因此网关侧无法为其余账号「制造」该活动，需要另一台物理机器。

**验证**
- 独立复现 issue #18 的四条结论（Windows + Docker Desktop / WSL2 后端，与作者的 Linux 环境逐条一致），并补两个对照：**`compose restart` 与 `stop/start` 在身份效果上等价**、**host 网络下重建同样会换**；
- PR #17 的回归在本地实跑：10 checks / 10 passed / 0 failed。

## v1.2.9

**✨ 新增功能**：无

**🐛 问题修复**
- **设置页「复制 API Key」会闪退到登录界面**（点复制 → 弹出登录遮罩、复制失败）：前端把 HTTP **403** 与 **401** 等同处理，而`/settings/reveal` 在「面板仍使用默认密码」时返回 403 是**正确的安全守卫**（默认口令等于没有口令，不该交出明文 Key）。现改为**分流**：401 仍跳登录；**403 保留会话**，并把服务端消息原样提示（「面板仍在使用默认密码……请先在「设置 → 面板密码」修改密码，再查看明文 Key」）。

**🎨 体验优化**
- 切换出口失败、领福利失败两处从阻塞式 `alert()` 改为与全站一致的 `toast()`（文案未变；按钮不再被对话框吊住，失败后可立即重试）。

**⚠️ 其他变更**
- 清理无调用方的死代码：`filterRealm()`、`window.REALM_FILTER`、三条 `.realm-filter` CSS（原先引用已不存在的元素）。
- 为 `throwPanelAuthError` 补充参数契约注释（传 `parsedBody` 即视为响应流已被消费）。
- 文档：上一版 changelog 已改为四类格式。

**验证（本轮）**
- 前端：离线仿真矩阵（401/403 × getJSON/postJSON/downloadExport）**403 违反数 0 / 401 违反数 0**，且**改动前同一脚本会红**（违反数各 3）——断言有效；
- 真实端到端：临时实例 + 账号副本，默认密码登录 → `/settings/reveal` 实测 403 且 body 带可操作消息，前端能读到；
- 静态：语法解析 OK、82 个内联 handler / 57 个被调函数无缺失、残留计数全 0；
- 时序：alert→toast 那处的 `finally` 语义经独立分析**确认无竞态**。

**已知行为（非缺陷）**：默认面板密码下，唯一被挡的就是「复制明文 Key」——这是刻意设计；`/panel/password` 没有默认密码守卫，所以**不会锁死**，改密后即可复制。

## v1.2.8

> 本版**无功能变更**（v1.2.7 之后仅有测试与文档改动）。

**✨ 新增功能**：无 ・ **🐛 问题修复**：无 ・ **🎨 体验优化**：无

**⚠️ 其他变更**

- **issue #16 复查**：把双方标注的「未覆盖形态」全部测掉——长链 13 帧、三种反转（散文里讨论标记 / 前缀被打断 / 完整标记后接散文）、代理对半截、双重转义、`\u63a` 分帧、窗口边界 16/17/18、暂存上限内外、与结构化直传的交互——**全部符合预期，无新问题**。
- **修正一处判断**：此前以为「暂存窗口只是优化项」（把 `_MARKER_HOLD_WINDOW` 压到 1 时没有任何断言变红）。实测**不成立**——当帧**在标记中间被切开、且前面带散文**时，窗口=1 会**真实泄漏**。已补上该形态的回归断言：窗口值再被误改小会立即变红。
- 顺带补齐 3 条暂存上限断言 + 17 条复查断言。
- 基线：**587 checks, 584 passed, 0 failed, 3 skipped, exit 0**。

## v1.2.7

**修复 issue #16：回读守卫的两个边界形态**（作者附了可直接运行的探针脚本，两个形态都稳定复现）

- **B1｜散文与 marker 落进同一个 delta**：暂存此前只在「整帧以 marker 开头」时启动。而服务端的 delta 切分不可控——散文与 marker 被合并进同一帧时整帧会被透传。现在改为**在首个完整标记处切分**：标记之前的散文**立即发出**（不拖首字延迟），标记及其后进入暂存；同时补上**跨帧**处理（帧尾 17 字符内的标记真前缀暂留到下一帧再判定，证伪立即补发）。
- **B2｜散文与 marker 分帧**：此前行为已正确，本次保留并补断言。
- **B3｜截断落在未写完的 `\uXXXX` 转义里**：`_json_array_prefix_ok()` 原先只认「语法错误点落在末尾」，而 `Invalid \uXXXX escape` 的报错位置在转义串中间 → 判为不可扩展 → 不吞。现已补上该分支（并按实测修正：Python 3.13 的 `JSONDecodeError.pos` 指向 `u` 本身而非反斜杠，两个位置都要检查）。
- **顺手修掉一个既有缺陷**（被本次 hold 扩展放大了触发面）：收尾帧的重发路径存的是**原帧**，当该帧内容已进入暂存（被吞或已补发）后，流末重发会把同一段再输出一次——表现为**正常文本以 `[` 或标记前缀结尾时，下一帧内容被输出两次**（如 `数组：\n[` + `1,2,3]` → `数组：\n[1,2,3]1,2,3]`）。这条是质检环节专门构造「误伤用例」时抓到的。
- **新增暂存上限保护**：窗口固定为最长标记长度 − 1（17 字符），不会因正常文本长期以 `[` 开头而无限增长（有断言覆盖「长文本分帧后全部放行不丢字」）。
- 新增 16 条断言（五形态 / 判据单元 / 误伤边界 ×6 / 窗口常量 / 非流式 / Responses / 暂存上限）+ 3 条变异反证（其中把本次修复退化为原样 → 重复输出立刻复现，证明回归网有效）。基线：**587 checks, 566 passed, 0 failed, 3 skipped, exit 0**。

## v1.2.6

**新增：结构化工具历史直传（针对 issue #8 / #11 类「模型复述内部信封」的根治方向）**

- **背景**：issue #8 / #11 的根因是模型会模仿它在上下文里反复看到的格式。协议层评估的结论是——**换信封无法根治**（旧信封会随历史重放，换一次只多一层守卫），真正的对策是「**不再把工具历史文本化**」。本版本实现了这条路线。
- **实测依据**（14 个真实上游请求的验证矩阵）：现有 SSE 端点**接受**结构化工具历史且**模型确实消费**——覆盖 3 轮连续工具链、结构化与旧文本信封**混合历史**、2 个并行 `tool_calls`、`tool_call_id` 不匹配、缺 tool 结果等形态。
- **行为**：`flatten_messages()` 新增结构化分支——
  - `tool` 消息 → `{role:"tool", tool_call_id, content}`；
  - `assistant` + `tool_calls` → `{role:"assistant", content:"", tool_calls:[…]}`；
  - **`content` 一律用空字符串、绝不用 `null`** —— 实测 `null` 会让上游转换层丢掉那条 assistant 消息，DeepSeek / Kimi 会直接拒绝（`Messages with role 'tool' must be a response to a preceding message with 'tool_calls'`）；
  - `tool_call_id` 不齐备时**整请求回退文本化**（fail-safe：宁可少用，也不发畸形请求）。
- **开关与灰度**：`QD_STRUCTURED_TOOL_HISTORY` 三态（`on` / `off` / `auto`，默认 **auto**）。auto 只在「**CN 出口 + provider 白名单（qwen / glm / gm4 / gm5）+ id 齐备**」时启用；DeepSeek / Kimi 与全部 INTL 暂排除在 auto 之外（前者待补端到端、后者该通道未实测），需要时可 `on` 强制放行。
- **保留全部回读守卫**：其他 provider 仍在文本化，结构化路径异常也有兜底（既有守卫一行未改）。
- **端到端实测**（网关实跑）：auto + Qwen3.8-Flash 结构化路径 → 200 且答对，日志打出 `structured tool history: ON`；`off` 一键回退同样 200 且答对。
- 回退：`QD_STRUCTURED_TOOL_HISTORY=off`。

**新增：上游「藏在 200 信封里」的错误现在可见**

- 上游有时把错误塞在 HTTP 200 的 SSE **内层**（如 `provider_error` 内嵌 `invalid_request_error`）。此前网关没有任何提示，上游收紧时用户只会看到 200 的空正文。
- 现在：内层错误按类别（content_policy / rate_limit / invalid_request / auth / other）**进程级计数**，并按类别**首次**打一条 WARN（`upstream error hidden in HTTP 200 envelope: …`）；计数可通过 `/usage/perf` 的 `inner_errors` 字段查看。
- **零行为改动**：清洗、透传、重试、错误映射全部原样（有断言固定）。

**测试**：587 checks / 541 passed / 0 failed / 3 skipped（本轮新增 13 条断言 + 2 条变异反证）。

## v1.2.5

**新增：历史工具结果削减（降低模型复述内部信封的概率 + 大幅省 token）**

- **背景**：issue #8 / #11 的根因是「模型会模仿它在上下文里反复看到的格式」。协议层评估的结论是——换信封**不能根治**（旧信封仍会随历史重放，换一次只多一层守卫），真正的对策是「不再文本化」或「大幅减少暴露」。本版本做后者。
- **行为**：历史里的工具结果（`[工具结果<name>]…`）在进入上游上下文前做**首尾保留 + 中间省略**：
  - **只削历史**：最后一条 user/system 消息之后（即当前轮）的工具结果**全文保留**——模型正在用的内容不受影响；
  - **信封前缀逐字节不变**（`[工具结果<name>]\n`），因此既有回读守卫与历史兼容性不受影响；
  - ≤4.2 KB 的结果**不削**（绝大多数 `ls` / `cat` / `grep` 输出都在此列）；完整 JSON ≤8 KB 也不削（保可解析）；
  - 省略处插入明确标记（含「需要完整内容请重新执行该命令」的行动指引），避免模型把截断处当成残缺数据去揣测。
- **开关**：`QD_TOOL_RESULT_KEEP` —— 正数 = 单侧保留字符数；`0` / `off` / `false` = 完全关闭（回退旧行为）；非法值走默认 **2000**。
- **实测收益**：15 条 8 KB 工具结果 → 117 KB 降到 59 KB（**-49%**）；30 条 20 KB → 585 KB 降到 118 KB（**-80%**）。
- **已知代价**：diff 的中间 hunk、>8 KB JSON 的中段会被省略（靠省略标记 + JSON 双倍预算缓解）。
- 新增 10 条断言（削减逻辑 / 首尾保留 / 前缀不变 / 当前轮不削 / 短结果 / JSON / 开关三态 / 默认值）。当前基线：**587 checks, 510 passed, 0 failed, 3 skipped, exit 0**。

## v1.2.4

**修复 issue #12：Docker 里提取出的 UMID 组件跑不起来（alpine 缺 glibc 兼容层）**

- **现象**：v1.2.3 的组件提取在宿主上正常，但在 alpine 镜像里**执行失败**——`exit 127`，观感像「文件不存在」，实际是组件在、执行位也对、**exec 失败**。
- **根因**（真 Docker 实测）：提取出的 `runtime-info` 是 **glibc 动态链接**的 ELF，依赖 `/lib64/ld-linux-x86-64.so.2` 与 `libstdc++.so.6`；而 alpine 是 musl，两者都不存在。
- **修复**：运行时阶段的 `apk add` 增加 `gcompat libstdc++ libgcc`（约 +3.1 MB）。实测：装包后同一二进制 `exit=0` 并返回真实身份；`docker build` 成功，容器内 `/app/umid/runtime-info` 可直接执行。
- **错误可见性**：`run_runtime_info()` 不再静默吞异常——「组件不存在」（正常降级，静默）与「组件在但跑不起来」（stderr 打一次 `[runtime-info] 无法执行 …` 并提示 alpine 需要 gcompat）现在**可区分**，同一类失败每进程只提示一次。
- **能力边界补充（社区实测证实）**：
  - Docker 里的身份是**容器级**而非机器级：同一容器内稳定，但**每新建一个容器就换一套身份**——重建容器等于换设备；需要身份稳定请复用容器（`docker start` / `docker compose start`，而不是 `-d --build`）。
  - `vmInfo.brand` **不是固定值**（`Docker` / `KVM` 都出现过），判断虚拟化只看 `isVm` / `vmTypeCode`。
  - v1.2.1 当时标注「未验证」的「真机器头可能额外解锁设备定向活动」**已被证实**：装上兼容层让组件真正可执行后，国内版「新人任务」活动从**不可见变为可见**。

**修复 issue #11：`[工具结果…]` 标记的回读缺失**

- **现象**：网关把 tool 消息序列化成 `[工具结果<name>]\n<内容>` 写进历史，但回读守卫只覆盖 `[assistant 请求调用工具]`。模型照格式复述时，这段内部文本会**原样透传**给终端用户，并进入会话历史继续被放大（与 issue #8 同机制）。
- **修复**：新增 `TOOL_RESULT_MARKER` / `TOOL_RESULT_CLOSE` 常量（写入侧改为引用常量，**写出的字节逐字节不变**），并按 issue #9 的同形态守卫补上回读——覆盖两种回声形态：网关原格式与**模型自造的开闭对**（`[工具结果] … [工具结果结束]`）。三条 finalize 路径全覆盖；截断 / 只有开标记同样吞掉；讨论该标记的散文、未声明 tools 一律不吞（fail-open 保持）。
- **取舍**：吞掉（空 content + `finish_reason=stop`），**不**还原成 tool 消息——后者需要 `tool_call_id` 配对，属协议层重构，注释里已记下这条更根本的方向。

**测试与验证**：`python _test_qoder.py` → **587 checks, 500 passed, 0 failed, 3 skipped, exit 0**（新增 [31] 段 21 条 + 3 条变异反证）；**Docker 实测**：3 次构建 + 容器内验证，含「容器内直接执行组件拿到真实身份」与「同容器稳定 / 新容器换身份」的对照。

## v1.2.3

**从 `@qoder-ai/qodercli` 提取内嵌 UMID 组件，让 Linux / Docker 部署也能拿到真实机器身份**

- **背景**：v1.2.2 把国际版「没有真身份」如实标成了已知限制（`machine_headers=omitted` + `hint`）。本期把「拿不到真身份」这件事本身解决掉——官方 CLI 的发行包里其实**内嵌了各平台的原生 UMID 组件**。
- **提取原理（实测）**：`@qoder-ai/qodercli` 是纯 JS bundle，原生组件以 **base64 字符串字面量**内嵌在 `package/bundle/qoder-worker-runtime.mjs` 里（共 11 个长字面量）。新增 `_install_umid.py`：下载官方 tarball（校验 sha512 integrity）→ 扫描长 base64 字面量 → **按格式与架构识别**（解析 ELF `e_machine` / Mach-O `cputype`）→ 选中当前平台的组件 → 原子落盘并 `chmod 755`。零第三方依赖，**不需要 node 作为运行时依赖**。
- 实测候选清单：`WASM × 5 · Mach-O x86_64/arm64 × 2 · ELF x86-64/aarch64 × 2 · PE x86-64 × 2`；Linux x64 选中 652,488 B、arm64 选中 660,624 B（另注：本机桌面端的 `runtime-info.exe` 与包内 PE 组件同为 480,752 B，互为佐证）。
- **接入**：`runtime_info_exe()` 在 POSIX 上新增查找路径 `$QD_UMID_DIR/runtime-info` 与 `<repo>/umid/runtime-info`（桌面客户端路径仍优先，**Windows 行为逐字不变**）；调用契约与官方桌面端二进制完全一致（`prod --account-stdin`），调用侧无需改动。
- **容器**：Dockerfile 改为**构建期多阶段提取**（builder 用 `python:3.11-alpine`，不需要 node；31 MB 的 npm 包不进最终镜像，只多 652 KB 组件），按 `TARGETARCH` 支持 amd64/arm64；提取失败不阻断构建（退化为 `omitted` 而非报错），脚本本身也 COPY 进镜像以便运行期补救。
- **实测验证**：在 WSL 里真实执行提取出的组件，返回 `machineToken` / `machineType` / `machineCode` / `vmInfo`；连续三次调用身份字段逐字节一致。
- **明确的能力边界（重要）**：该组件输出的是**机器级**身份，与 `account` 参数无关——不同账号 / 空账号 / 不同 `HOME` / 不同 `XDG_CONFIG_HOME` 全部返回**同一个** `machineToken`，且组件没有 CLI 开关、环境变量或状态文件可以影响它。所以本期**解决的是「Linux 部署拿不到真身份」**，**不是**「同一台机器多账号都能领」——后者是上游按设备去重的策略，需要不同的机器（详见「已知限制」）。⚠️ Docker 场景的补充实测（issue #12）：容器里该身份实为**容器级**——同一容器内稳定，但**每个新建容器会换一套身份**，重建容器等于换了设备；详见「已知限制」第 1 节。
- 新增 [30] 段 21 条断言（格式/架构识别、平台映射、体积启发式、base64 扫描、integrity 与 magic 双重校验、幂等、端到端全链路、跨模块契约）+ 3 条变异反证。当前基线：**482 checks, 479 passed, 0 failed, 3 skipped, exit 0**。

## v1.2.2

**跟进 issue #10 的补充报告：把「机器身份」的状态与实际行为分开表达**

- **背景**：issue #10 作者补充了三条第三方独立来源，确认「国内版只需宿主头（`UA` / `Cosy-ClientType` / `Cosy-Version` + `Authorization`），不需要 `Cosy-Machine*`」；同时指出**国际版要的是真实 UMID 身份**——派生假值不能替代，无官方组件时应**如实提示为已知限制**，而不是发假头去撞。
- **状态与行为分离**：`desktop_headers()` 新增 `machine_headers_state`（`native` = 本次发送了原生机器头；`omitted` = 本次未发送任何 `cosy-machine*` 头）。它与原有的 `identity`（身份来源：`runtime-info` / `derived`）是**正交**的两个维度——只看 `identity` 会把「省略」误读成「降级但仍可用」，这正是原作者指出的误导点。
- **活动平台接口暴露该状态**：`campaigns()` 新增只读键 `machine_headers`；既有键的语义与顺序不变。
- **国际版已知限制提示**：当 realm 为 intl 且本次未发送机器头时，新增 `hint` 字段给出明确文案（「国际版要求真实 UMID 机器身份……活动列表可能不可见、领取可能失败，这是已知限制，不等于今天没有活动」）；国内版不提示（省略在国内版是**正确行为**）。
- `_diag_campaign.py` 的「活动平台」行改为同时打印身份来源与本次机器头状态，并输出 hint。
- 新增 [29] 段 8 条断言（状态字段与**实测头集合**同时校验，防止状态与行为脱钩）+ 2 条变异反证（忘记收窄 / 国内版误提示 都会变红）。
- 当前基线：**482 checks, 458 passed, 0 failed, 3 skipped, exit 0**。
- **关于从 `@qoder-ai/qodercli` 提取 UMID 组件**：本轮**未实现**——属于引入第三方二进制的新能力（分发合规、提取链路的可维护性、以及「每台设备每日仅 1 个国际版账号可领」的上游约束），需单独评估。

## v1.2.1

**修复 issue #9：截断的工具调用回声不再透传给用户**

- v1.2.0 的回读守卫要求整段正文**恰好**是 `[assistant 请求调用工具]` + 合法 JSON 数组；当模型把数组**写到一半就被截断**（流未收尾）时判据失配，网关会把这段内部协议文本当普通正文透传——客户端直接显示给用户，且这段文本进入会话历史后会让模型继续引用它、滚雪球式产生更多回声（issue #9 的真实样本：Qwen3.8-Flash / intl / stream=true，255 字符数组从未闭合）。
- 新增**严格的截断吞掉判据**（三条件同时成立）：正文以标记开头、本次请求声明了 tools、标记之后仍是 JSON 数组字面量的**前缀**。命中时**吞掉正文**（输出空 content，`finish_reason` 保持 `stop`）并记一条 WARN 日志；完整可解析的回声仍走原有还原路径，而**讨论该标记的普通回复、marker 后接散文、未声明 tools** 三种情形一律不吞（fail-open 保持）。
- 三条 finalize 路径全覆盖：流式 chat 收尾、非流式聚合（含非流式 Responses）、Responses 流式；新增 [27.5] 段 12 条回归断言。

**修复 issue #10：derived 机器身份会让「每日领取 100 Credits」被整条过滤**

- **现象**（Linux/Docker 部署）：容器里没有官方风控桥 `runtime-info.exe` → 取不到原生身份 → 网关仍发出**派生**的 `cosy-machine*` 六头 → 服务端把**可领取状态**的 Credits 活动整条过滤掉，列表只剩详情类活动；旧 sash 兜底又是 `DISABLED`，于是 `run_checkin` 返回 `ok=True`、面板显示「签到成功」，而额度一动不动。
- **根因**由 issue 作者逐头隔离实测定位：六个机器头**单独任一个**出现时活动可见，**全套一起发**即被过滤（去掉 `machinetoken` 或 `machineid` 后恢复可见）。
- **修法**：`desktop_headers()` 仅在**原生身份可用**（`machineToken` 非空）时才发送六个 `cosy-machine*` 头；derived 分支一律不发，但 `User-Agent: Qoder` / `cosy-clienttype` / `cosy-version` 照发（这三个是服务端展示活动所必需的）。native 分支行为逐字未动——不影响「真机器头可能额外解锁设备定向活动」这条尚未验证的路径。（**后续证实**：该路径已被社区实测验证——装上 glibc 兼容层让组件在容器里真正可执行后，国内版「新人任务」活动从不可见变为可见；见 issue #12 与「已知限制」。）
- **顺带修掉一个相邻死逻辑**：身份自愈分支写成 `source == "native"`，而该字段的真实取值只有 `runtime-info` / `derived`（源码里根本不存在 `native`）→ 自愈从未触发过。现统一到常量 `MACHINE_IDENTITY_NATIVE`，消费端保留历史别名兼容；探针实证：修复前同输入只发 1 次请求（死逻辑），修复后发 2 次并强制刷新身份。
- `_diag_campaign.py` 的「活动平台」行补一条直白提示——当前身份是否携带机器头，避免再出现需要翻源码才能解释的「签到成功但没到账」。
- 新增**双向**回归断言：derived 分支**不得**出现六头、原生分支**必须**齐发。

**其余修复（本轮全项目体检所得）**

- **Responses 链路重试丢上下文**：流内信封重试与瞬时错误重开会话时，传给上游的是**原始 Responses payload**，而请求体构造只读 `messages`——重试即变成空会话（观感是「重试后模型失忆」）。现改为传转换后的 chat 请求。
- **Responses 流式失败没有终态事件**：上游信封错误时只关流、不发终态，客户端会一直等。现补发 `response.failed`（含 `error.code/message`），并让事件序号在重开后**续号**而非回退到 0。
- **明文 Key 暴露面收窄**：面板仍在用默认密码（默认 `admin`）时，`GET /settings/reveal` 直接返回 403 并提示先改密码——局域网模式下这原本等于「任何人登录后都能读走明文 API Key」；启动时检测到「默认密码 + LAN 监听」会额外打印 WARN。
- **防风控间隔对齐**：`/accounts/checkin` 此前用 0.4s 间隔且账号之间零停顿，与模块声明的「>= 1.0s」不符；现统一为 `CHECKIN_MIN_GAP = 1.0`，账号之间按「与上次调用耗时叠加」等待，失败路径同样计入（try/finally）。活动平台内部的领取间隔也改为可透传（未指定时默认 1.0s，成功/被挡/失败三类结果统一等待）。
- **Pro 福利包计数虚增**：已领取（409/ALREADY）此前照样按 +1800 累进批量 `credit_added`；现区分「真实新增」与「本就已领」，已领取记 0 并给出 `already_claimed` 状态，旧字段全部保留。
- **调度器重启不再重复签到**：`enabled/last_run_time/next_run_time` 等状态此前只在内存，进程重启就会重新触发一次真实领取；现状态落盘（`<账号目录>/scheduler/state.json`，原子写），并对启动巡检加「当日最多补签一次」闸门（mark-before-act，崩溃不重放）。
- **`/v1/completions` 不再静默错答**：该路径此前被路由白名单放行却按 Chat 语义处理，legacy `prompt` 请求会被当成空会话转发；现明确返回 404 并给出改用 `/v1/chat/completions` 的指引。
- **模型数据来源可见**：`GET /v1/models` 新增只读字段 `catalog_source`（`external-json` / `embedded-frozen`）——三套快照的价格口径并不相同，降级即换一套峰谷价，现在一眼可见。
- **看板三处修复**：账号空态按钮此前调用未定义的 `startLogin()`（点击即 ReferenceError），已接回完整的设备授权链路；「各账号用量透视」因渲染目标 DOM 被删而永久空白，已补回；运行日志改为增量追加（此前每 2 秒全量重建最多 2000 行 DOM）。
- **测试基线转绿**：离线测试此前因把官方 fixture 目录硬编码成本机 `%TEMP%` 路径而固定 RED（`EXIT=1`）。现改为 `QD_TEST_FIXTURE_DIR` 优先 + 多候选自动探测，缺 fixture 时逐条打印 `[SKIP]` 与候选清单（绝不静默），退出码只由 FAIL 决定；AES-256 两条纯算法 KAT 移出 fixture 分支、永远执行。当前基线：**TOTAL 482 checks, 434 passed, 0 failed, 3 skipped, exit 0**。
- **容器交付面**：`Dockerfile` 的 `CMD` 不再写死 `--port 8790`，改为尊重 `PORT` / `HOST` 环境变量（compose 配置真正生效）；镜像内补入 `_test` / `_diag` / `_verify` / `_refresh` 验证脚本，容器里具备自检面。
- **工程卫生**：新增 `.gitattributes`（`*.py` / `*.json` 固定 LF）消除刷新脚本写回时的行尾噪声；`.dockerignore` 排除 `.team/`、`accounts/`、`usage/`；清理 `_IDENTITY_KEYS`、`VOLATILE_FIELDS` 两处死代码；修正 `derive_id` 文档与实现不一致（实测输出 32 位十六进制，实现保持不变以免既有账号机器身份突变）；为 `tempKey`（64-bit 熵）与 RSA-1024 包裹补上风险说明与「不可单方面更改」的理由。

## v1.2.0

**修复 issue #8：工具调用被模型以文本复述后泄漏为正文**

- 成因：`flatten_messages()` 会把历史里的 assistant `tool_calls` 序列化成 `[assistant 请求调用工具]` + JSON 数组交给上游模型当上下文；长会话里模型会**照格式复述**成普通正文（没有结构化 tool_calls），客户端于是把这段内部 JSON 当正文显示、本轮工具调用也不执行。
- 新增**严格守卫的「回读」**：整段正文必须**恰好**是 `[assistant 请求调用工具]` + JSON 数组（可带 ``` / ```json 围栏）；数组非空、每项 `name` 为非空字符串、`arguments` 为合法 JSON（对象自动规范化）；请求声明了 tools 时**工具名必须命中声明集合**——讨论该标记的普通回复、数组后带多余文字、未声明工具名一律不误判。
- **三条链路全覆盖**：
  - 非流式 `chat.completions`：聚合时还原为结构化 `tool_calls` 并从正文移除，`finish_reason` 置为 `tool_calls`；
  - 流式 `chat.completions`：marker 前缀先压住不发（跨增量分段也能认），识别成功改发 `tool_calls` 增量帧并改写收尾帧 `finish_reason=tool_calls`；一旦被证伪立即把暂存整段补发为正文，**绝不吞字**（fail-open）；
  - `Responses API`：流式转成 `function_call` 输出项，不再回显为 `output_text`。
- **不改写入侧序列化格式、不注入/篡改给上游的提示词**；另对上游 `content` 为 parts 列表等非字符串形态做了防御性展开。
- 新增 [27] 段 16 条回归断言（形态守卫、围栏、跨增量、证伪补发、未声明工具名、三条链路端到端）。

## v1.1.9

**合并 PR #7**（by @XD06）：修复信封层 `403 (10605 / isQueued)` 排队满时不停重试同一受限账号的问题——
- 信封层捕获 `UpstreamStatus` 时解析上游 `retryAfterSeconds`，对触发排队的**账号+模型**精确冷却（默认 30s）并解绑会话亲和，下一次请求自动轮换；
- 未向客户端吐出字节前允许 401/403/429 重开换号（重试预算与"已输出"保护保持不变）；
- 死会话（TOKEN_EXPIRE）经信封层同样停用账号；非流式 / Responses / Chat 流式三处接入；
- 新增 [26] 段回归断言（模型级/账号级冷却、解绑、停用、重开语义与内容审核不重试）。

## v1.1.8

**活动显示中文（含自动探测到的活动）**
- 活动名/说明直接取**服务端下发的中文文案** `placements[].content.zh.title/description`（如「发布 Qoder 站点，免费领取奶茶免单卡」「每天领 100 Credits」「限时福利，专业版/高级版首月Credits翻倍」），并附官方 `detailUrl` 详情页；服务端未带文案时用内置中文兜底表（按 campaignKey 前缀/奖励类型）；任务中心、聚合视图、签到日志全部中文。

**按钮语义（明确分工）**
- 签到与福利中心：**「领取全部福利」** = 每日签到/限时活动（含券类兑换码）+ Pro 福利包，一次点完（均幂等）；旁边保留 **「仅领 Pro 福利包」**；
- 账号面板：**「每日签到」只做每日签到领积分**（Credits 类活动 + 旧 sash 接口兜底），**不触碰券/兑换码类活动与 Pro 包**（`/accounts/checkin` 走 `run_checkin(only_daily=True)`）；
- 每日签到行只统计"可领取的 Credits 类"活动（详情类 VIEW_DETAILS 不再被算作签到奖励）。

**关于"自动把任务做完"（边界说明）**
- 站点类任务（如 `sites_first_use`）在官方客户端里是 **agent 工具链**（`prepare_site` → `get_publish_status` → `publish_site`，由客户端打包上传并维护 release 状态），网关作为 HTTP 反向代理**无法执行客户端工具**，也不应伪造发布以套取活动权益（活动条款明确禁止异常手段、官方按"人"去重并可收回）；
- 网关做到的是：**探测到任务已完成就自动领取**（含名额发完的次日 10:00 重试）；任务未完成的账号会在任务中心明确标注「需先在官方桌面端完成新人任务（成就 xxx）」并给出活动页入口——在桌面端做完一次后无需再管，领取全自动。

## v1.1.7

**全部账号视图（批量）= 按活动聚合 + 每账号资格明细**
- `uid=all` 时任务中心不再只显示某一个账号，而是**把所有账号的活动聚合成行**，每行标注：
  `可领 x/N：账号…；已领 x/N：…；名额发完 x/N：…；需先完成任务 x/N：…；无资格(不在定向) x/N：…`——一眼看清**谁有资格、谁没有**；
- 批量领取仍**逐账号独立判断**，只对服务端判定可领的账号发起（无资格的账号不会被打扰）；
- 卡片改为合计口径（「合计积分余额（全部账号）」「连续签到（批量视图）」）。

**兑换码/券按账号展示**
- 签到与福利中心新增**「已领取的兑换码 / 券（按账号）」面板**：账号（昵称+uid+区域）· 活动 · 兑换码，带**复制**按钮与**活动页/二维码**入口；`/tasks` 的 `summary.codes` 亦按账号输出。

**语义澄清（写进规则）**
- 领取**只认服务端"任务已完成"状态**，网关不会去代做任务（如桌面端「站点」发布）；
- 是否有资格**完全以上游返回为准**：只有上游真的返回 `SAME_PERSON_ALREADY_CLAIMED` 才退避（该账号记 6 小时冷却）；同机器上若上游未判为同一人，各账号**正常各自领取**（签到同理）。

## v1.1.6

**新增：券/兑换码类活动也纳入领取与呈现（如「奶茶免单卡」act-20260928-620）**
- 任务中心为**非 Credits 奖励**的活动单独成行（奖励列显示「兑换码 ×1」），状态与官方前端一致：
  `CLAIMABLE` 待领奖 / `CLAIMED` 已领（**回显兑换码**）/ `REDEMPTION_CODE_OUT_OF_STOCK` 今日名额已发完（每日 10:00 刷新）/ `ACHIEVEMENT_NOT_COMPLETED` 需先完成新人任务 / 活动已结束；
- 一键签到日志新增 `⏳ 名额已发完，次日 10:00 后自动重试` 与 `🎟 兑换码：xxxx`（可扫码兑换）；
- **兑换码落盘持久化**（`campaignCodes`），网关重启后不会丢；领取成功但上游还在发码时标记「兑换码发放确认中」；
- 领取失败码映射中文说明（名额发完 / 数据未完成 / 风控拦截 / 活动未开始）。
> 说明：该活动的任务条件（桌面端「站点」发布 AI 站点 → `sites_first_use`）目前由官方客户端完成，网关只负责**条件已满足后的自动领取与呈现**；每日名额发完时按官方口径次日 10:00 后再领（整点排程 21:00 会自动重试）。

## v1.1.5

**性能：看板切换视图不再等几秒**
- `/tasks` 的 5 个上游查询由**串行改并行**，并加 20 秒面板短缓存（签到/领取写操作后立即失效）——实测 **8.9s → 热 0.00s**；
- 账号文件写入加锁（并行刷新时同一 tmp 路径会被并发写坏）；
- 前端：切视图改为**并行加载** + **按区域缓存即时渲染**（模型库/成长任务），实测**账号池/模型库 28–53ms 刷新**（原来数秒）；「最近请求」等也随之即时更新。

**修复**
- **签到后不再跳回国际版视图**：`initRealm()` 原来会把视图重置为"网关默认出口"，签到流程调用它导致视图跳区；改为保留当前视图；
- **本机虚拟化检测 404/检测失败**：旧网关进程没有 `/diag/vm` 路由，前端现在明确提示"需重启网关"而不是显示 404。

**新增**
- **国际版/国内版视图分区**：账号池列表与签到中心的账号下拉只列当前视图区域的账号（`/tasks?realm=`）；积分按区域分别显示；
- **项目新版本检测**：`GET /update/check` 对比 GitHub 最新 release，设置页「运行信息 · 新版本检测」自动检查并提示升级（可「立即检查」，结果缓存 6 小时）。

## v1.1.4

**新增**
- `_diag_campaign.py` 增加**本机虚拟化状态**体检：官方风控桥 vmInfo（是否虚拟机/平台/风险评分/类型码）+ 本机交叉校验（CPU 型号、系统制造商、虚拟化驱动、VBS/HVCI），全中文输出并附误报提示。
- 看板「签到与福利中心」新增**本机虚拟化检测**卡片（同源，可「重新检测」）；新增 `GET /diag/vm` 接口（面板鉴权）。

**修复**
- `/diag/*` 路由纳入面板鉴权（此前未带令牌也能读取）。

## v1.1.3

**修复**
- 活动平台的机器身份**会随时间轮换**：长缓存会拿到过期身份，导致活动列表被过滤、漏领。改为短缓存（120s）+ 签到前强制刷新 + 列表被过滤时刷新重试一次。
- 修复活动列表 404 判定的 `available` 标志（重构后误判）。

**验证**
- 多账户实测（3 账号 / 双区域）：国内版 +100 Credits（余额 594→694）、国际版账号 +100（余额 0→100）、另一国际版账号当日无可领取项——三个账号互不影响，批量签到 `ok=true`。

## v1.1.2

**修复：签到仍领不到（双区域）**
- 活动列表还依赖**真实机器身份**（`Cosy-MachineToken/Type/Code`）：官方桌面端用自带风控桥
  `resources/umid/runtime-info.exe prod --account-stdin` 取值，派生假身份会让服务端**静默过滤掉**
  「每日领取 100 Credits」这类设备定向活动。网关改为调用同一个官方二进制取真值（缓存 6h，
  `QD_NATIVE_IDENTITY=0` 可关闭），失败回退派生值并标注 `identity=derived`。
- 实测：国内版真实领取成功 **+100 Credits（余额 594 → 694）**；国际版与官方桌面端列表逐条一致。

**确认：`reasoning_effort` 按官方档位表归一化**
- 命中/`none` 原样透传；未命中取最近合法档位（同距偏向模型默认档）；有 `thinking_config` 但无档位表的模型
  不下发无效档位；没有 `thinking_config` 的路由模型（`auto`）原样透传；Chat Completions 与 Responses 两条路径都已覆盖。

## v1.1.1

**签到真正做到「能领到」（双区域）**
- **根因**：官方活动平台 `/sash/api/v1/me/campaigns` 必须携带**桌面端请求头**（`Cosy-ClientType: 10` + `Cosy-Version` + 机器头 + `UA: Qoder`）；此前网关用普通 openapi 头请求，服务端不报错但返回**空活动列表** → 看板上永远是"无活动"；
- **现在直接领取**：对 `CLAIMABLE` 的 Credits 活动 `POST /sash/api/v1/me/campaigns/{id}/claim`（逆向自官方 `growth-page/activity-iframe` 页面 JS，**官方幂等**：已领返回 `replayed=true`，不会重复发放）；任务中心显示「可领取 100 Credits —— 点『一键签到』自动领取」/「今日已领取 +100 Credits」；
- 旧 sash 签到接口降级为兜底（仅在仍开放时附一行历史状态）。

**思考档位 `reasoning_effort` 归一化（回答"Qwen3.8-Flash 传档位没反应"）**
- 字段名确认无误（官方 0.4.3 SDK 参数表即 `reasoning_effort`），但**各模型合法档位不同**，且**上游对不支持的档位静默忽略**：`qfmodel` 只认 `low/medium/xhigh`（传 `high` 无效）、`dfmodel` 只认 `low/high/max`（传 `medium/xhigh` 无效）、`qmodel` 只有开/关；
- 网关改为按官方目录的档位表归一化：未命中取最近合法档位（`dfmodel`：`medium→high`、`xhigh→max`；`qfmodel`：`high→medium`、`max→xhigh`）并记日志；无档位表的模型不再下发无效档位；`none` 始终可用于关闭思考；兼容 `reasoning.effort` 与 `thinking.effort/level`。

## v1.1.0

**适配 0.4.3 双区桌面端**
- `cosy-version` 跟随官方 0.4.3 客户端更新为 `1.1.64`（实测模型列表与推理均正常）；
- 国际版推理主机改为官方候选主选 `api1.qoder.sh`，并新增 `api2`/`api3` 自动故障切换（传输层失败即换域名，签名只覆盖 path 不受影响）；
- 双区模型目录快照随新版客户端刷新（价格倍率 / 上下文窗口 / 思考默认档 / `is_sensitive`），新增 `_refresh_catalog.py` 一键随客户端更新并打印差异；
- 修复 `model_entry` 两处计费语义缺陷：低谷价改为官方定义 `峰价 × 折扣`（旧实现把当前价当谷价）、`off_peak` 元数据在非低谷时段也会下发（旧实现整块丢失）。

**修复 issue #1（签到没效果 / 国际版也有签到活动）**
- 签到能力改为**运行时探测**（404/405/410 记不可用，6 小时 TTL 后自动重探），不再按区域硬编码；国际版接口实测 404、国内版旧活动实测 `status=DISABLED`，都会给出明确原因而不是"点了没反应"；
- 接入官方新活动平台 `GET /sash/api/v1/me/campaigns`（双区域可用），任务中心「限时活动」行呈现 `showCampaign/claimable/campaignUrl`；「每日领取 100 Credits」的领取动作在官方客户端内完成（服务端下发 JS，网关只做状态呈现，不执行远端代码）。

**修复 issue #2（DeepSeek-Flash 偶发调用失败）**
- `reasoning_content` 兼容层此前两处断点：按客户端名字前缀 `deepseek` 判定（写 `dfmodel` 时不生效）→ 改按上游 key 判定；`flatten_messages` 压平时无条件丢弃该字段（兼容层"写了但从没发出去"）→ 改按目标模型保留；
- 瞬时故障容错：同账号 1s/2s 重试、HTTP200 建流后信封投 418 时重开上游、短冷却期间"等待续上"而非误报 429。

**HTTP 帧层**
- 流式响应改用 `Transfer-Encoding: chunked` 并以 `0\r\n\r\n` 正确收尾（不再 `Connection: close`），连接可复用；
- 上游长首字延迟期间每 5 秒发送 SSE 注释心跳 `: ping`（`QD_SSE_HEARTBEAT` 可调）；
- 新增 `/ping`（及 `/healthz`、`/livez`、`/readyz`）免鉴权探活端点，另附 `_diag_gateway.py` 自检脚本（默认仅回环，`--allow-remote` 走 SSRF 校验）。
