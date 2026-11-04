package codearts

// capture.go 补采/探测的结果契约（C-CA-CAPTURE@1 的**类型部分**）。
//
// T1 只定义类型与枚举，不发起任何浏览器操作（浏览器实现属 T2）。
// 类型定在这里的原因：面板（T2）与续期决策（T3）都要消费同一个结构，
// 放在 codearts 包才能保证两边看到的字段与 JSON 名完全一致。

// CapturePhase 补采探测的阶段（固定枚举）。
type CapturePhase = string

const (
	// CapturePhaseProbed 已登录，且至少有一个带绝对过期时间的票据。
	CapturePhaseProbed CapturePhase = "probed"
	// CapturePhaseSessionOnly 已登录，但票据全是会话级/未观测，没有绝对截止时间。
	CapturePhaseSessionOnly CapturePhase = "session_only"
	// CapturePhaseLoginDead 注入 Cookie 后控制台仍判定未登录（会话已死）。
	CapturePhaseLoginDead CapturePhase = "login_dead"
	// CapturePhaseNoCookie 账号里压根没有可用 Cookie。
	CapturePhaseNoCookie CapturePhase = "no_cookie"
	// CapturePhaseError 环境错误（找不到 Chrome / 端口不通 / 超时等），不得改写号池。
	CapturePhaseError CapturePhase = "error"
)

// CaptureResult 一次补采/探测的结果。
//
// 失败语义（面板与日志必须照此展示，不得把"没采到"说成"已过期"）：
//   - Phase=error：环境/网络问题，Message 原文，不改号池；
//   - Phase=login_dead：会话已死，只能人工重新登录；
//   - Phase=session_only：登录态有效，票据没有绝对截止时间 → TicketSource=session，
//     TicketDeadline 必须为空，但**允许续期**；
//   - Phase=probed：TicketDeadline 为最早票据到期时刻，TicketSource=cookie。
type CaptureResult struct {
	OK                 bool     `json:"ok"`
	Phase              string   `json:"phase"`
	Cookies            []Cookie `json:"cookies,omitempty"`
	TicketDeadline     string   `json:"ticket_deadline,omitempty"`
	TicketSource       string   `json:"ticket_source,omitempty"`
	ExpiresKnownCount  int      `json:"expires_known_count"`
	SessionCookieCount int      `json:"session_cookie_count"`
	Message            string   `json:"message,omitempty"`
	// Evidence 固定格式的核对串，形如：
	//   SSOTGC=session;SSOJTC=1767225600;CASTGCLOW=unknown;expires_known=37/55;session_cookies=14
	Evidence string `json:"evidence,omitempty"`
}
