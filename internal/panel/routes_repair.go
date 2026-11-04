package panel

import "net/http"

// registerRepairRoutes 集中注册「三项缺陷修复」在契约中新增的端点（T11）。
//
// 这里只做接线：handler 实现分别位于 codearts_api.go（C-CA-API@1 B/C）、
// codearts_auto.go（C-CA-API@1 D）、ide_qoder.go（C-QD-API@1 C/D/E）与
// taskcenter.go（C-WB-DIAG@1）。所有端点一律套 withAuth（Bearer <api_key>），
// 与既有 /panel/api/* 同口径。
//
// ⚠️ 只注册契约新增的模式：codearts/accounts、qoder/checkin、
// qoder/checkin-status、tasks/scan_all 等既有路由已在 routes() 注册过；
// Go 1.22 ServeMux 对同一「方法+路径」重复注册会直接 panic，不要在这里重复。
func (p *Panel) registerRepairRoutes(mux *http.ServeMux) {
	// C-CA-API@1 B/C/D：CodeArts 验活 / 补采到期时间 / 静默续期。
	mux.HandleFunc("POST /panel/api/ide/codearts/keepalive", p.withAuth(p.codeartsKeepaliveHTTP))
	mux.HandleFunc("POST /panel/api/ide/codearts/probe", p.withAuth(p.codeartsProbeHTTP))
	mux.HandleFunc("POST /panel/api/ide/codearts/renew", p.withAuth(p.codeartsRenewHTTP))
	// C-CA-SLOT@1：身份槽列举 / 重置（换设备身份，必须 confirm=true）。
	mux.HandleFunc("GET /panel/api/ide/codearts/slots", p.withAuth(p.codeartsSlotsHTTP))
	mux.HandleFunc("POST /panel/api/ide/codearts/slot_forget", p.withAuth(p.codeartsSlotForgetHTTP))

	// C-QD-API@1 D/E：每账号设备/出口配置的读与写。
	mux.HandleFunc("GET /panel/api/ide/qoder/egress", p.withAuth(p.qoderEgressGet))
	mux.HandleFunc("POST /panel/api/ide/qoder/egress", p.withAuth(p.qoderEgressSet))
	// C-QD-API@1 C：逐日领取历史账本。
	mux.HandleFunc("GET /panel/api/ide/qoder/claim_history", p.withAuth(p.qoderClaimHistory))

	// C-WB-DIAG@1：按账号输出任务列表诊断。
	mux.HandleFunc("GET /panel/api/tasks/diag", p.withAuth(p.tasksDiag))
}
