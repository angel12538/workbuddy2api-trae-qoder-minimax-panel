// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/minimax"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/traechat"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder
	// RequestLog 请求指标与归档（nil = 对应接口返回 501）。
	RequestLog *reqlog.Recorder

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// TraeEnabled 是否启用「Trae 签到」板块（config 的 trae.enabled）。
	TraeEnabled bool
	// TraeDataDir Trae Work 助手的数据目录（含 data/checkin_accounts.json）。
	// 空 = 回落 %APPDATA%\TraeWorkAssistant。
	TraeDataDir string
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// mmLoginMu/mmLogins 进行中的 MiniMax 设备码授权会话（state → 会话）。
	// 与上面的 WorkBuddy 登录会话分开存：两套 OAuth 服务器、两套端点、两套
	// 错误语义，混在一起迟早串味。
	mmLoginMu sync.Mutex
	mmLogins  map[string]*minimax.DeviceSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState

	// Trae 模型通道（trae: 前缀）账号池与目录缓存；nil = 未启用。
	// 由 cmd/server 启动时经 SetTraeChat 注入，面板据此展示并行占用与冷却。
	traeChat    *traechat.Pool
	traeCatalog *traechat.CatalogCache

	// MiniMax Code 模型通道（minimax: 前缀）。nil = 未启用。
	minimaxClient *minimax.Client
}

// SetMinimax 注入 MiniMax Code 通道（供面板模型页展示其模型目录）。
func (p *Panel) SetMinimax(c *minimax.Client) { p.minimaxClient = c }

// SetTraeChat 注入 Trae 模型通道（供面板展示/诊断）。
func (p *Panel) SetTraeChat(pool *traechat.Pool, cat *traechat.CatalogCache) {
	p.traeChat = pool
	p.traeCatalog = cat
	if pool != nil {
		// 对话号池遇到 401（JWT 过期）时，异步尝试「从 Trae 客户端提取新令牌」自救：
		// 客户端在线时会自己刷新 JWT，提取成功后号池按文件 mtime 自动重载。
		pool.OnUnauthorized = func(userID string) {
			if r := p.TraeSyncFromClient(false); r.OK {
				log.Printf("panel: Trae 401 自救：已从客户端换新令牌 uid=%s", userID)
			}
		}
	}
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
		mmLogins: map[string]*minimax.DeviceSession{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.withAuth(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAuth(p.importCockpit))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAuth(p.accountPause))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAuth(p.accountResume))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("GET /panel/api/trae/accounts", p.withAuth(p.traeAccounts))
	// Trae 积分：实时查上游优先，凭证不可用时回落本地快照（助手 remaining_credits.json）。
	p.mux.HandleFunc("GET /panel/api/trae/credits", p.withAuth(p.traeCredits))
	p.mux.HandleFunc("POST /panel/api/trae/checkin", p.withAuth(p.traeCheckin))
	// Trae 账号添加：OAuth 授权码 + 本地回调（与上面 WorkBuddy 的 login/* 是两套体系）。
	p.mux.HandleFunc("POST /panel/api/trae/login/start", p.withAuth(p.traeLoginStart))
	p.mux.HandleFunc("GET /panel/api/trae/login/poll", p.withAuth(p.traeLoginPoll))
	// Trae 账号添加（推荐路线）：面板自拉浏览器 + CDP 取凭证。
	// 授权页路线在「认证中」会卡住、且复用浏览器登录态导致多账号不可用，故另开这条。
	p.mux.HandleFunc("POST /panel/api/trae/browser/start", p.withAuth(p.traeBrowserStart))
	p.mux.HandleFunc("GET /panel/api/trae/browser/poll", p.withAuth(p.traeBrowserPoll))
	p.mux.HandleFunc("POST /panel/api/trae/browser/stop", p.withAuth(p.traeBrowserStop))
	// Trae 令牌续期：refresh_token → ExchangeToken，或绑定的持久 profile → 无头浏览器。
	// JWT 只有 8 小时，靠它自动续期实现「长期免登录」。
	p.mux.HandleFunc("POST /panel/api/trae/refresh", p.withAuth(p.traeRefresh))
	p.mux.HandleFunc("POST /panel/api/trae/client_sync", p.withAuth(p.traeClientSync))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("GET /panel/api/usage/official", p.withAuth(p.usageOfficial))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	// 模型排队与冷却：WorkBuddy 池内模型级冷却 + Trae 模型请求限额（Qoder 无该接口）。
	p.mux.HandleFunc("GET /panel/api/model_queue", p.withAuth(p.modelQueue))
	p.mux.HandleFunc("GET /panel/api/ide/trae/limits", p.withAuth(p.traeLimits))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))

	// Qoder 本地 IDE 账号（读 h号池 / 添加 / 切号）
	p.mux.HandleFunc("GET /panel/api/ide/qoder/accounts", p.withAuth(p.qoderAccounts))
	p.mux.HandleFunc("POST /panel/api/ide/qoder/add", p.withAuth(p.qoderAdd))
	p.mux.HandleFunc("POST /panel/api/ide/qoder/switch", p.withAuth(p.qoderSwitch))
	p.mux.HandleFunc("POST /panel/api/ide/qoder/remove", p.withAuth(p.qoderRemove))
	p.mux.HandleFunc("POST /panel/api/ide/qoder/checkin", p.withAuth(p.qoderCheckin))
	p.mux.HandleFunc("POST /panel/api/ide/qoder/checkin_switched", p.withAuth(p.qoderCheckinSwitched))
	p.mux.HandleFunc("GET /panel/api/ide/qoder/checkin-status", p.withAuth(p.qoderCheckinStatus))
	p.mux.HandleFunc("GET /panel/api/ide/qoder/usage", p.withAuth(p.qoderUsage))
	p.mux.HandleFunc("POST /panel/api/ide/qoder/renew", p.withAuth(p.qoderRenew))
	p.mux.HandleFunc("GET /panel/api/ide/qoder/device", p.withAuth(p.qoderDevice))

	// MiniMax Code：多账号号池 + 每日签到 + 积分（token 级操作，无需切号）
	p.mux.HandleFunc("GET /panel/api/ide/minimax/accounts", p.withAuth(p.minimaxAccounts))
	p.mux.HandleFunc("POST /panel/api/ide/minimax/add", p.withAuth(p.minimaxAdd))
	p.mux.HandleFunc("POST /panel/api/ide/minimax/add_manual", p.withAuth(p.minimaxAddManual))
	p.mux.HandleFunc("POST /panel/api/ide/minimax/remove", p.withAuth(p.minimaxRemove))
	p.mux.HandleFunc("POST /panel/api/ide/minimax/checkin", p.withAuth(p.minimaxCheckin))
	p.mux.HandleFunc("POST /panel/api/ide/minimax/renew", p.withAuth(p.minimaxRenew))
	p.mux.HandleFunc("POST /panel/api/ide/minimax/login/start", p.withAuth(p.minimaxLoginStart))
	p.mux.HandleFunc("GET /panel/api/ide/minimax/login/poll", p.withAuth(p.minimaxLoginPoll))

	// 豆包（Doubao 桌面客户端）：多账号签到池（Cookie 凭据 + CDP 浏览器通道）
	p.mux.HandleFunc("GET /panel/api/ide/doubao/accounts", p.withAuth(p.doubaoAccounts))
	p.mux.HandleFunc("POST /panel/api/ide/doubao/add_extract", p.withAuth(p.doubaoAddExtract))
	p.mux.HandleFunc("POST /panel/api/ide/doubao/add_cookie", p.withAuth(p.doubaoAddCookie))
	p.mux.HandleFunc("POST /panel/api/ide/doubao/remove", p.withAuth(p.doubaoRemove))
	p.mux.HandleFunc("POST /panel/api/ide/doubao/checkin", p.withAuth(p.doubaoCheckin))
	p.mux.HandleFunc("GET /panel/api/ide/doubao/status", p.withAuth(p.doubaoStatus))

	// Trae IDE 本地账号（读号池 / 添加 / 切号）
	p.mux.HandleFunc("GET /panel/api/ide/trae/accounts", p.withAuth(p.traeIdeAccounts))
	p.mux.HandleFunc("POST /panel/api/ide/trae/add", p.withAuth(p.traeIdeAdd))
	p.mux.HandleFunc("POST /panel/api/ide/trae/switch", p.withAuth(p.traeIdeSwitch))
	p.mux.HandleFunc("POST /panel/api/ide/trae/remove", p.withAuth(p.traeIdeRemove))

	// WorkBuddy 账号（国内版 / 国际版）
	p.mux.HandleFunc("GET /panel/api/ide/workbuddy/accounts", p.withAuth(p.workbuddyAccounts))
	p.mux.HandleFunc("POST /panel/api/ide/workbuddy/start", p.withAuth(p.workbuddyStart))
	p.mux.HandleFunc("POST /panel/api/ide/workbuddy/poll", p.withAuth(p.workbuddyPoll))
	p.mux.HandleFunc("POST /panel/api/ide/workbuddy/add", p.withAuth(p.workbuddyAdd))
	p.mux.HandleFunc("POST /panel/api/ide/workbuddy/switch", p.withAuth(p.workbuddySwitch))
	p.mux.HandleFunc("POST /panel/api/ide/workbuddy/remove", p.withAuth(p.workbuddyRemove))
	p.mux.HandleFunc("POST /panel/api/ide/workbuddy/sync-pool", p.withAuth(p.workbuddySyncPool))
	p.mux.HandleFunc("GET /panel/api/ide/workbuddy/usage", p.withAuth(p.workbuddyUsage))
	p.mux.HandleFunc("POST /panel/api/ide/workbuddy/checkin", p.withAuth(p.workbuddyCheckin))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 与 server 包同口径的 Bearer 鉴权（经 httpauth 常量时间比较）；
// api_key 为空时放行。密钥经 livecfg 快照读取：面板里改了 api_key，下一个请求
// 即用新值（无需重启）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, p.apiKey()) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// expiringSoonWindow 返回调度器当前生效的快过期路由窗口；测试面板无调度器时返回 0。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	// 用 WithPaused 而不是 CountsDetailed：后者把暂停号并进 disabled（/status 的
	// 「不可用」口径），而概况卡片要分开展示（issue #125）。
	total, healthy, cooling, disabled, paused, inFlightFull := p.cfg.Pool.CountsDetailedWithPaused()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"paused":          paused,
		"in_flight_full":  inFlightFull,
		"accounts":        p.cfg.Pool.List(),
		// model_locks 模型级限流全清单（哪些模型不能用、锁了几个号、还要锁多久）：
		// 与 accounts 的账号池视图互补，前端「模型锁池」表直接渲染。无锁时为 null。
		"model_locks": p.cfg.Pool.ModelLockView(),
	})
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// requestMetrics 返回进程内请求指标、最近 100 条与归档状态。
func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.RequestLog.Snapshot())
}

// requestLogs 从 JSONL 归档读取最近请求；limit 默认 200、最大 1000。
// 支持按 outcome/account/model/client_ip/user_agent 过滤（字符串字段为包含匹配）
// 与 from/to 时间区间（闭区间，unix 秒或 RFC3339）——面板「运行日志」的筛选框、
// 来源查询与「今天 / 自定义区间」都走这里。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	q := r.URL.Query()
	rows, err := p.cfg.RequestLog.ReadArchive(limit, reqlog.Filter{
		Outcome:   q.Get("outcome"),
		Account:   q.Get("account"),
		Model:     q.Get("model"),
		ClientIP:  q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		From:      parseTimeParam(q.Get("from")),
		To:        parseTimeParam(q.Get("to")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 空结果回 []（而不是 JSON null）：前端把 null 与"归档关闭"混在一起会走错分支，
	// 显示成不满足筛选条件的最近请求。
	if rows == nil {
		rows = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 与 /v1/models 同口径的双域输出：CN 域模型加 "cn:" 前缀、global 域加 "global:" 前缀
// （gateway 路由协议，前端显示的 id 就是调用时要填的完整 model 值）。
// 各域独立探测、独立容错：某域无可用账号则整域跳过；两域全空时才报错
// （有错误明细回 502，一个账号都没有回 503）。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	var fetchErrs []string

	// CN 域：有可用 CN 账号才查（此前无条件 Pool.Pick()+FetchModels——选中 global
	// 账号时打 CN 端点必然失败，混合池表现为偶发 502，纯 global 池必炸）。
	if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
			infos, err := p.cfg.Upstream.FetchModels(acct)
			if err != nil {
				fetchErrs = append(fetchErrs, "cn: "+err.Error())
			} else {
				for _, mi := range infos {
					out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
				}
			}
		}
	}

	// global 域：路由开关开且有可用 global 账号才查（独立目录端点，FetchGlobalModelInfos；
	// Upstream.GlobalEnabled 是探测侧同一道闸，与 main 装配的 config global.enabled 一致）。
	if p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				if len(infos) == 0 {
					fetchErrs = append(fetchErrs, "global: 上游未返回可用模型")
				} else {
					efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
					for _, mi := range infos {
						out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
					}
				}
			}
		}
	}

	// Qoder 域：本机 Qoder 已登录时，追加 Qoder 官方模型目录（静态口径，见 qoderModelEntries）。
	if snap, err := ideauth.CaptureQoderSnapshot(); err == nil && snap != nil && snap.Email != "" {
		out = append(out, qoderModelEntries()...)
	}

	// MiniMax Code 域：客户端已登录时追加其模型目录（动态拉取，失败则不列）。
	if p.minimaxClient != nil {
		if ms, err := p.minimaxClient.Models(r.Context()); err == nil {
			for _, m := range ms {
				e := map[string]any{
					"id":                 "minimax:" + m.ID,
					"name":               m.Name,
					"vendor":             "MiniMax",
					"context_length":     m.Context,
					"max_output_tokens":  m.Output,
					"supports_tool_call": m.ToolCall,
					"supports_reasoning": m.Reasoning,
					"tags":               []string{"minimax"},
				}
				out = append(out, e)
			}
		}
	}

	if len(out) == 0 {
		if len(fetchErrs) > 0 {
			writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(fetchErrs, "; "))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// qoderModelEntries 返回 Qoder 域的模型目录（静态口径，来源 docs.qoder.com/zh/cli/model）。
// Qoder 客户端不暴露公开模型目录接口，故按官方文档内置；倍率即文档里的 price_factor。
func qoderModelEntries() []map[string]any {
	type qm struct {
		id, name, credits, def, vendor, desc string
		efforts                              []string
		ctx                                  int
		canDisable, reasoning                bool
	}
	list := []qm{
		{"Qwen3.8-Max", "Qwen3.8-Max", "0.5×", "medium", "阿里云", "千问最新旗舰，2.4T 参数，支持推理/多模态/思考开关，200K-1M 上下文", []string{"low", "medium", "xhigh"}, 1000000, true, true},
		{"Qwen3.7-Max", "Qwen3.7-Max", "0.5×", "medium", "阿里云", "智能体能力大幅跃升，可自主处理长程复杂任务，支持多模态/思考开关", []string{"low", "medium", "high"}, 1000000, true, true},
		{"Qwen3.7-Plus", "Qwen3.7-Plus", "0.1×", "medium", "阿里云", "推理能力、效率与多模态体验全面提升，高性价比", []string{"low", "medium", "high"}, 1000000, true, true},
		{"Kimi-K3", "Kimi-K3", "0.8×", "high", "月之暗面", "2.8T 参数，面向软件工程/知识工作/深度推理，支持多模态", []string{"low", "high", "max"}, 1000000, false, true},
		{"Kimi-K2.7-Code", "Kimi-K2.7-Code", "0.3×", "high", "月之暗面", "面向长上下文编码，256K 上下文，提供 Fast 模式", nil, 256000, false, false},
		{"GLM-5.3", "GLM-5.3", "0.6×", "high", "智谱", "擅长复杂系统工程与长程任务，支持推理/多模态", []string{"low", "high", "max"}, 1000000, false, true},
		{"GLM-5.2", "GLM-5.2", "0.6×", "high", "智谱", "擅长复杂系统工程与长程任务，支持思考开关", []string{"high", "max"}, 1000000, true, true},
		{"DeepSeek-V4-Pro", "DeepSeek-V4-Pro", "0.8×", "high", "深度求索", "擅长复杂推理、代码生成与工程任务，支持思考开关", []string{"high", "max"}, 1000000, true, true},
		{"DeepSeek-V4-Flash", "DeepSeek-V4-Flash", "0.3×", "high", "深度求索", "快速推理、低成本、能力均衡，支持思考开关", []string{"low", "high", "max"}, 1000000, true, true},
		{"MiniMax-M3", "MiniMax-M3", "0.2×", "medium", "MiniMax", "原生多模态感知与前沿编码能力，1M 上下文", nil, 1000000, false, false},
		{"Auto", "Auto（智能路由）", "1.0×", "", "Qoder", "自动选择最适合当前任务的模型，平衡性能与成本", nil, 1000000, false, false},
		{"Ultimate", "Ultimate（专家级）", "1.6×", "", "Qoder", "专家级深度推理与思考能力，适合复杂系统设计", nil, 1000000, false, false},
		{"Performance", "Performance（高性能）", "1.1×", "", "Qoder", "高级推理与高质量输出，适合核心功能实现/重构", nil, 1000000, false, false},
		{"Efficient", "Efficient（高性价比）", "0.3×", "", "Qoder", "标准推理、低成本，适合基础代码生成与日常问答", nil, 1000000, false, false},
	}
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		e := map[string]any{
			"id":                   "qoder:" + m.id,
			"name":                 m.name,
			"credits":              m.credits,
			"vendor":               m.vendor,
			"description":          m.desc,
			"context_length":       m.ctx,
			"supported_efforts":    m.efforts,
			"can_disable_thinking": m.canDisable,
			"supports_reasoning":   m.reasoning,
			"supports_images":      true,
			"supports_tool_call":   true,
			"tags":                 []string{"qoder"},
		}
		if m.def != "" {
			e["default_effort"] = m.def
		}
		out = append(out, e)
	}
	return out
}

// panelModelEntry 构造单个模型条目（两域共用）：id 带 realm 前缀（调用值即显示值），
// context_length / max_output_tokens 走四级查找链，effort 档位按 realm 域取
// EffortListing（远端权威 ∪ 静态兜底表）——与 /v1/models 同一口径，两侧不再漂移。
func panelModelEntry(realm string, mi upstream.ModelInfo, remoteEfforts []string, remoteDefault string, httpc *http.Client) map[string]any {
	entry := map[string]any{
		"id":                   realm + ":" + mi.ID,
		"name":                 mi.Name,
		"default_effort":       mi.DefaultEffort,
		"supported_efforts":    mi.Efforts,
		"can_disable_thinking": mi.CanDisableThinking,
		"supports_reasoning":   mi.SupportsReasoning,
		"supports_images":      mi.SupportsImages,
		"credits":              mi.Credits,
		"description":          mi.Description,
		"tags":                 mi.Tags,
		"vendor":               mi.Vendor,
		"is_default":           mi.IsDefault,
		"supports_tool_call":   mi.SupportsToolCall,
		"only_reasoning":       mi.OnlyReasoning,
		"reasoning_effort":     mi.ReasoningEffort,
		"reasoning_summary":    mi.ReasoningSummary,
	}
	// 限时优惠（modelPromotions）：credits 是牌价，promo_* 是当前生效折扣
	//（WorkBuddy 客户端显示的就是这个生效价）。前端据此显示「生效价+标签+划线牌价」。
	if mi.PromoFactor != nil {
		entry["promo_factor"] = *mi.PromoFactor
		entry["promo_credits"] = mi.PromoCredits
	}
	if mi.PromoLabel != "" {
		entry["promo_label"] = mi.PromoLabel
	}
	if mi.PromoNote != "" {
		entry["promo_note"] = mi.PromoNote
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}
	return entry
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPause 暂停选号：账号退出选号候选，但**照常参与**签到 / 活跃上报 / 保活 /
// 余额刷新。与 disable 的区别：不写 reason、不清冷却域、不重置计数——账号是「临时
// 让位」而非「判死」，点「恢复选号」即可立刻回到池子（无需重登或解冻）。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Pause(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: pause uid=%s（暂停选号，保号任务照常）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountResume 解除暂停选号（幂等，对未暂停账号为空操作）。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Resume(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: resume uid=%s（恢复参与选号）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	checkinDone := false
	// 企业版无签到体系（上游 400 code 10001「企业账号不支持该操作」）：不发起该调用，
	// 端点退化为「刷新额度」——企业额度走 get-enterprise-user-usage 口径（见 upstream）。
	// 前端对企业号不渲染「签到」按钮；此处是 API 侧防御（外部脚本/旧缓存前端仍可能调用）。
	if a.IsEnterprise() {
		checkinMsg = "企业账号无签到体系（已跳过签到，仅刷新额度）"
	} else if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
		// 幂等拒绝同样是「今日已签」，标记后按钮显示「已签」。
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckinDone(uid)
			checkinDone = true
		}
	} else {
		p.cfg.Pool.NoteCheckinDone(uid)
		checkinDone = true
	}
	resp := map[string]any{"ok": true, "checkin_done": checkinDone}
	if a.IsEnterprise() {
		resp["enterprise"] = true
	}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：更新余额与到期快照，不触碰冷却状态。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunCheckinNow()
	log.Printf("panel: 手动全量签到已触发（含猫猫旅行）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunTravelNow()
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunActivityNow()
	log.Printf("panel: 手动全量活跃上报已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunKeepaliveNow()
	log.Printf("panel: 手动全量保活已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	p.cfg.Scheduler.RunBalanceRefreshNow()
	p.syncNicknames()
	log.Printf("panel: 手动全量余额刷新完成")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// syncNicknames 手动刷新时的昵称同步（issue #94：上游改名免重登）。
// 只在面板手动「刷新」路径调用——后台余额定时器不触发（用户明确要求资料接口
// 仅手动触达）。逐号拉 /console/account，只取 nickname（手机号等敏感字段在
// upstream.FetchAccountProfile 内即被丢弃）；单号失败静默跳过，不打断余额刷新
// 的既有结果。
func (p *Panel) syncNicknames() {
	type job struct {
		uid string
		a   *auth.Auth
	}
	var jobs []job
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.AccessTokenValue() != "" {
			jobs = append(jobs, job{uid: st.UID, a: a})
		}
	}
	if len(jobs) == 0 {
		return
	}
	var (
		mu      sync.Mutex
		updated int
		failed  int
		sem     = make(chan struct{}, 3)
		wg      sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nick, err := p.cfg.Upstream.FetchAccountProfile(j.a)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			if p.cfg.Pool.SetNickname(j.uid, nick) {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	if updated > 0 || failed > 0 {
		log.Printf("panel: 昵称同步：更新 %d 个，失败 %d 个（未变化不计数）", updated, failed)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。统计窗口三选一：
//   - from/to（unix 秒）：显式区间，用于「今天」与「自定义」——区间由浏览器按
//     本地时区算好再发，服务端时区与浏览器不一致时「今天」才不会被算错；
//   - hours：滚动窗口（默认 72，上限 1440=60 天），卡片汇总/按域/按账号/按模型/
//     时序**全部**按该窗口统计；显式 hours=0 表示全部历史（含 90 天前折叠出的日桶）；
//   - 都不给：等同于 hours=72（保持旧调用方行为）。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	q := r.URL.Query()
	win := usage.Window{}
	win.From = parseTimeParam(q.Get("from"))
	win.To = parseTimeParam(q.Get("to"))
	if win.From.IsZero() && win.To.IsZero() {
		win.Hours = 72
		if v := q.Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				win.Hours = n
			}
		}
		if win.Hours > 1440 {
			win.Hours = 1440
		}
	}
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	var currentRate func(realm, model string) string
	if p.cfg.Upstream != nil {
		currentRate = p.cfg.Upstream.ModelRate
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.SnapshotWindow(win, nicks, currentRate))
}

// parseTimeParam 解析时间查询参数：unix 秒（前端默认）或 RFC3339（便于手工调
// 接口/写脚本）。空串与非法值都返回零值 = 该侧不设界，不报错——区间参数是可选
// 增强，拼错一个 from 不该让整页用量打不开。
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		// 兼容秒与毫秒（前端可能直接把 Date.now() 传上来）。
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
