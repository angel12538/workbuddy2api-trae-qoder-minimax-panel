package panel

// apicache.go 面板读接口的「短 TTL 缓存 + 在途合并」。
//
// 为什么需要（实测 2026-11-04，浏览器 Network 面板逐条量过）：
// 面板签到池首屏会并发打同一批慢接口 —— qoder/checkin-status 2.6s、
// qoder/usage 1.8s、workbuddy/usage 1.9s、minimax/accounts 1.2s、
// codearts/accounts 1.0s；而前端三条渲染链（号池列表 / 任务中心 / 积分速览）
// 各自独立请求一遍，于是同一个接口被打 4~5 次，每次都真打上游。
// 结果就是"进面板要加载很久"，而且**时间被并发放大了**。
//
// 这里在服务端补两道：
//   - **在途合并**：同一个 key 的并发请求只真正执行一次，其余等它的结果；
//   - **短 TTL 缓存**：TTL 内的重复请求直接复用上次结果。
//
// TTL 取 15s：额度/签到状态在签到后会变，缓存太久会让用户看到旧值；
// 15s 足够把"同一次首屏里的重复请求"全部合并掉，又不至于明显过期。
// 另外任何**写**请求（签到/切号/补采…）都会清空缓存（见 withAuth），
// 所以"点完签到立刻刷新"看到的永远是最新值。

import (
	"bytes"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/ideauth"
)

// apiCacheTTL 读接口缓存的默认有效期。
const apiCacheTTL = 15 * time.Second

// apiCacheTTLIDE IDE 号池读接口的缓存有效期（更长）。
//
// 为什么更长：这几个接口一次要打上游（实测 qoder/checkin-status 4.1s），
// 而它们查的是"今天签没签 / 还剩多少额度"这种**分钟级**数据 —— 用 15 秒的
// TTL 的话，后台预热完很快就过期，用户打开面板照样要干等。
// 90 秒 + 后台每 60 秒续热 ⇒ 缓存几乎永远是热的，用户打开即秒开。
//
// 过期风险有兜底：**任何写请求（签到/切号/补采…）都会清空整个缓存**
// （见 withAuth），所以"点完签到立刻刷新"拿到的永远是刚算出来的最新值。
const apiCacheTTLIDE = 90 * time.Second

// apiCacheWarmInterval 后台续热周期（必须 < apiCacheTTLIDE，否则缓存会周期性失效）。
const apiCacheWarmInterval = 60 * time.Second

// apiCacheEntry 一条缓存：body/status 是结果，ready 用于在途合并。
type apiCacheEntry struct {
	body   []byte
	status int
	ctype  string
	at     time.Time
	ready  chan struct{}
	done   bool // 结果是否已经算好（false = 正在算）
}

// apiCache 并发安全的缓存表。
type apiCache struct {
	mu sync.Mutex
	m  map[string]*apiCacheEntry
}

// panelCache 面板级缓存（一个 Panel 一个实例）。
var panelCache = &apiCache{m: map[string]*apiCacheEntry{}}

// capWriter 捕获 handler 的输出（状态码 / Content-Type / body）。
type capWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (c *capWriter) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}

func (c *capWriter) Write(p []byte) (int, error) { return c.body.Write(p) }

func (c *capWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

// apiCacheClear 清空全部缓存（任何写请求后调用）。
func apiCacheClear() {
	panelCache.mu.Lock()
	panelCache.m = map[string]*apiCacheEntry{}
	panelCache.mu.Unlock()
}

// apiCacheKey 用完整请求路径（含 query）作 key —— qoder/usage?email=… 这种
// 必须按参数分开，不能共用一份。
func apiCacheKey(r *http.Request) string {
	return r.URL.RequestURI()
}

// cachedRead 给一个读 handler 套上「在途合并 + 短 TTL 缓存」。
//
// key 相同 ⇒ 同一份结果；并发只执行一次；TTL 内复用。
func (p *Panel) cachedRead(ttl time.Duration, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := apiCacheKey(r)

		panelCache.mu.Lock()
		e, ok := panelCache.m[key]
		if ok {
			if !e.done {
				// 正在算：等它算完复用结果（**不能**当成过期条目删掉重建 ——
				// 那会让并发的同 key 请求各自真打一次上游，在途合并就失效了）。
				ready := e.ready
				panelCache.mu.Unlock()
				select {
				case <-ready:
					writeCached(w, e)
					return
				case <-r.Context().Done():
					return
				}
			}
			if time.Since(e.at) < ttl {
				// 命中且仍在 TTL 内：直接复用。
				ready := e.ready
				panelCache.mu.Unlock()
				<-ready
				writeCached(w, e)
				return
			}
			// 过期 → 丢弃，重算。
			delete(panelCache.m, key)
		}
		ne := &apiCacheEntry{ready: make(chan struct{})}
		panelCache.m[key] = ne
		panelCache.mu.Unlock()

		cw := &capWriter{header: http.Header{}}
		h(cw, r)

		ne.body = cw.body.Bytes()
		ne.status = cw.status
		if ne.status == 0 {
			ne.status = http.StatusOK
		}
		ne.ctype = cw.Header().Get("Content-Type")
		ne.at = time.Now()
		ne.done = true
		close(ne.ready)

		// 复制一份再写出去（cw 的 buffer 之后可能被复用）。
		body := make([]byte, len(ne.body))
		copy(body, ne.body)
		writeCachedRaw(w, ne.status, ne.ctype, body)
	}
}

func writeCached(w http.ResponseWriter, e *apiCacheEntry) {
	body := make([]byte, len(e.body))
	copy(body, e.body)
	writeCachedRaw(w, e.status, e.ctype, body)
}

func writeCachedRaw(w http.ResponseWriter, status int, ctype string, body []byte) {
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// WarmIDECaches 后台预热并**持续续热** IDE 号池那几个慢读接口。
//
// 背景：缓存是**进程内**的，服务刚重启时是空的 —— 用户第一次打开面板仍然要
// 等上游（实测冷启动 qoder/checkin-status 4.1s、逐号 usage 1.0s/个）。
// 这里在后台定期把这些接口算一遍塞进缓存，用户打开面板就是热的。
//
// 周期必须短于 apiCacheTTLIDE，否则缓存会在两次续热之间过期，
// 用户照样会撞上冷查询（实测：15 秒 TTL + 一次预热 = 白预热）。
//
// 只读：预热走的就是面板自己的读接口，不碰号池、不领积分。
func (p *Panel) WarmIDECaches(delay time.Duration) {
	go func() {
		time.Sleep(delay)
		warm := func(path string, h http.HandlerFunc) {
			cw := &capWriter{header: http.Header{}}
			r, err := http.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
			if err != nil {
				return
			}
			p.cachedRead(apiCacheTTLIDE, h)(cw, r)
		}
		warmSet := func() {
			warm("/panel/api/ide/qoder/checkin-status", p.qoderCheckinStatus)
			warm("/panel/api/ide/qoder/accounts", p.qoderAccounts)
			warm("/panel/api/ide/minimax/accounts", p.minimaxAccounts)
			warm("/panel/api/ide/codearts/accounts", p.codeartsAccounts)
			// 面板是**逐号**查额度的，冷启动时每个约 1.0s —— 逐个预热。
			if list, err := ideauth.ListQoderAccounts(); err == nil {
				for _, a := range list {
					if a.Email == "" {
						continue
					}
					warm("/panel/api/ide/qoder/usage?email="+url.QueryEscape(a.Email), p.qoderUsage)
				}
			}
			if list, err := ideauth.ListWorkBuddyAccounts(); err == nil {
				for _, a := range list {
					if a.UID == "" {
						continue
					}
					warm("/panel/api/ide/workbuddy/usage?uid="+url.QueryEscape(a.UID), p.workbuddyUsage)
				}
			}
		}
		warmSet()
		t := time.NewTicker(apiCacheWarmInterval)
		defer t.Stop()
		for range t.C {
			warmSet()
		}
	}()
}
