// codearts_slot.go CodeArts 身份槽（Identity Slot）的生命周期。
//
// 设计依据：evidence/huawei-client-extract-recon.md §H。
//
// 槽的物理结构：
//
//	<SlotRoot>/<SlotDir>/
//	├── profile/            ← Chromium --user-data-dir（设备身份的真正载体）
//	├── slot.json           ← 槽元数据（只读对账用，缺了不影响运行）
//	└── <SlotDir>.bak-<ts>  ← codeartsSlotForget 留下的可恢复备份
//
// 三条硬约束（与 §H.1 的实现不变量一一对应）：
//  1. 一个账号一个槽，不同账号绝不复用同一 SlotDir（I1）；
//  2. 任何浏览器动作只能在本账号的槽 profile 内执行，禁止回落
//     data/codearts-web 或系统默认 profile（I2）；
//  3. 清槽先备份成 .bak-<ts>，不直接删（I5）。
package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/codearts"
)

const (
	// codeartsSlotDirMode 槽目录权限：profile 里有会话 Cookie，只给当前用户。
	codeartsSlotDirMode = 0o700
	// codeartsSlotScanFileLimit 统计槽大小时最多看多少个文件。
	//
	// 用久的 Chromium profile 有几万个文件（Cache / Code Cache / leveldb），
	// 面板每次列举都全量遍历会把请求拖住；一万个已足够给出正确的量级。
	codeartsSlotScanFileLimit = 10000
)

// slotInfo 一个账号身份槽的对外视图（面板"身份槽"一栏用）。
//
// SlotDir 给的是**绝对槽目录**（= <SlotRoot>/<Account.SlotDir>），不是号池里
// 那个相对目录名：面板要让人能直接去资源管理器核对，相对名没法用。
type slotInfo struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	SlotID       string `json:"slot_id"`
	SlotDir      string `json:"slot_dir"`
	DeviceGUID   string `json:"device_guid"`
	PersonKey    string `json:"person_key"`
	ClientSource string `json:"client_source"`
	Exists       bool   `json:"exists"`
	ProfileBytes int64  `json:"profile_bytes"`
}

// codeartsSlotMeta 槽目录里 slot.json 的结构（§H.2 的落地子集）。
//
// 只记录能在面板上对账的字段；**不含**任何 Cookie / 票据 / token。
type codeartsSlotMeta struct {
	SlotID       string `json:"slot_id"`
	DeviceGUID   string `json:"device_guid"`
	PersonKey    string `json:"person_key"`
	ClientSource string `json:"client_source"`
	CreatedAt    string `json:"created_at"`
}

// codeartsSlotLockTable 每账号一把内存锁。
//
// 锁键是**账号 ID**（forget 会改 SlotID，用 SlotID 当键会导致锁键漂移）：
// 同一账号的"建槽 / 清槽 / 补采占槽"串行，不同账号互不阻塞。
//
// 只做内存互斥：跨进程（面板多实例）由 Chromium 自己对 user-data-dir 的
// 独占锁兜底，不在本次范围。
var codeartsSlotLockTable = struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}{locks: map[string]*sync.Mutex{}}

// codeartsSlotLockKey 槽锁键：账号 ID（稳定不变），没有 ID 时退回槽目录名。
func codeartsSlotLockKey(a *codearts.Account) string {
	if a == nil {
		return ""
	}
	if id := strings.TrimSpace(a.ID); id != "" {
		return id
	}
	if dir := strings.TrimSpace(a.SlotDir); dir != "" {
		return "dir:" + dir
	}
	return ""
}

// acquireCodeartsSlotLock 取该账号的槽锁，返回解锁函数。
//
// 必须 defer 立即调用返回的函数；键为空时不加锁（账号字段异常，但也不该
// 让整个调用失败）。
func acquireCodeartsSlotLock(key string) func() {
	if key == "" {
		return func() {}
	}
	codeartsSlotLockTable.mu.Lock()
	lock := codeartsSlotLockTable.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		codeartsSlotLockTable.locks[key] = lock
	}
	codeartsSlotLockTable.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

// ── 创建 / 查询 ─────────────────────────────────────────────────────────

// codeartsSlotDir 该账号身份槽的 Chromium profile 目录（绝对路径，已创建）。
//
// 首次调用即建槽：`<SlotRoot>/<SlotDir>/profile`（MkdirAll 0700）。"新建槽 =
// 新设备"——目录里的 Local State / Network Persistent State 由 Chromium 首次
// 启动时自行生成，绝不从别的槽复制（§H.4 / I3）。
//
// 并发：同一账号并发调用由 per-account 锁串行；MkdirAll 本身也是幂等的，
// 所以即使跨进程同时调用也只会得到同一个目录。
func (p *Panel) codeartsSlotDir(a *codearts.Account) (string, error) {
	unlock := acquireCodeartsSlotLock(codeartsSlotLockKey(a))
	defer unlock()
	return p.ensureCodeartsSlotDir(a)
}

// ensureCodeartsSlotDir codeartsSlotDir 的**无锁**实现。
//
// 单独拆出来给"已经持锁、还要覆盖整个浏览器生命周期"的调用方（补采）用：
// 同一把锁不可重入，那边必须先用 acquireCodeartsSlotLock 再调本函数。
func (p *Panel) ensureCodeartsSlotDir(a *codearts.Account) (string, error) {
	if a == nil {
		return "", errors.New("账号为空，无法定位身份槽")
	}
	// EnsureSlot 是幂等的就地补全（SlotID/SlotDir/DeviceGUID 全是确定性派生）。
	a.EnsureSlot()
	dir := a.SlotProfileDir()
	if dir == "" {
		return "", fmt.Errorf("账号 %s 的槽目录为空（SlotDir 未派生）", a.ID)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("解析槽路径 %s 失败: %w", dir, err)
	}
	if err := os.MkdirAll(abs, codeartsSlotDirMode); err != nil {
		return "", fmt.Errorf("创建槽目录 %s 失败: %w", abs, err)
	}
	if err := codeartsWriteSlotMetaOnce(a, abs); err != nil {
		return "", fmt.Errorf("写槽元数据失败: %w", err)
	}
	return abs, nil
}

// codeartsSlotExists 该账号的槽 profile 是否真的存在。
//
// 存在 ≠ 可免登录：profile 只是设备身份容器，会话凭据仍在号池 Cookie 里
// （见 codearts.Account.Slot 的注释）。
func (p *Panel) codeartsSlotExists(a *codearts.Account) bool {
	if a == nil {
		return false
	}
	unlock := acquireCodeartsSlotLock(codeartsSlotLockKey(a))
	defer unlock()
	return a.SlotExists()
}

// codeartsSlotsOverview 列举号池里每个账号的身份槽。
//
// 读号池失败时返回 nil（调用方只用于展示；不能让一个坏文件把面板打挂）。
func (p *Panel) codeartsSlotsOverview() []slotInfo {
	pool, err := codearts.LoadPool()
	if err != nil {
		log.Printf("[codearts] 列举身份槽失败（读号池）: %v", err)
		return nil
	}
	out := make([]slotInfo, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		if a == nil {
			continue
		}
		out = append(out, p.codeartsSlotInfo(a))
	}
	return out
}

// codeartsSlotInfo 单个账号的槽视图。
func (p *Panel) codeartsSlotInfo(a *codearts.Account) slotInfo {
	unlock := acquireCodeartsSlotLock(codeartsSlotLockKey(a))
	defer unlock()

	a.EnsureSlot()
	profileDir := a.SlotProfileDir()
	exists := codeartsDirExists(profileDir)
	var bytes int64
	if exists {
		bytes = codeartsProfileBytes(profileDir)
	}
	label := a.Label
	if label == "" {
		label = a.Nickname
	}
	return slotInfo{
		ID:           a.ID,
		Label:        label,
		SlotID:       a.SlotID,
		SlotDir:      filepath.Dir(profileDir),
		DeviceGUID:   a.DeviceGUID,
		PersonKey:    a.PersonKey,
		ClientSource: a.ClientSource,
		Exists:       exists,
		ProfileBytes: bytes,
	}
}

// codeartsProfileBytes 槽 profile 的占用大小（最多扫 codeartsSlotScanFileLimit
// 个文件；被占用/无权限的条目跳过，不影响其它文件）。
func codeartsProfileBytes(root string) int64 {
	var total int64
	count := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if count >= codeartsSlotScanFileLimit {
			return filepath.SkipAll
		}
		count++
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// codeartsWriteSlotMetaOnce 第一次建槽时写 slot.json（已存在则不动）。
//
// 不覆盖已存在的文件：DeviceGUID 一旦被写进槽，就必须和号池里的值保持一致；
// 每次调用都重写只会让"这个目录到底属于哪个设备"变得不可信。
func codeartsWriteSlotMetaOnce(a *codearts.Account, profileDir string) error {
	slotDir := filepath.Dir(profileDir)
	path := filepath.Join(slotDir, "slot.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	meta := codeartsSlotMeta{
		SlotID:       a.SlotID,
		DeviceGUID:   a.DeviceGUID,
		PersonKey:    a.PersonKey,
		ClientSource: a.ClientSource,
		CreatedAt:    time.Now().Format(time.RFC3339),
	}
	buf, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	// 临时文件 + rename：两个 goroutine 同时首建时，最终只会有一个完整文件。
	tmp, err := os.CreateTemp(slotDir, ".slot.json.tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, werr := tmp.Write(buf); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return werr
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(name)
		return cerr
	}
	if rerr := os.Rename(name, path); rerr != nil {
		_ = os.Remove(name)
		return rerr
	}
	return nil
}

// ── 重置（换身份） ───────────────────────────────────────────────────────

// codeartsSlotForgetConfirmError 校验调用方是否带上了显式确认（confirm=true）。
//
// 放在这里而不是塞进 codeartsSlotForget：方法只管"做成这件事"，"要不要做"
// 是 HTTP 层的职责（本任务不改路由，故不注册 handler）。后续接线时一行即可：
//
//	if err := codeartsSlotForgetConfirmError(req.Confirm); err != nil { writeErr(...); return }
//
// 返回的文案可以直接回给前端，说明为什么必须确认。
func codeartsSlotForgetConfirmError(confirm bool) error {
	if !confirm {
		return errors.New("重置身份槽会换掉该账号的设备身份（DeviceGUID 变化），" +
			"需要用户显式确认（confirm=true）")
	}
	return nil
}

// codeartsSlotForget 重置该账号的身份槽 —— **仅供用户显式"重置该号身份"**。
//
// 语义（用户原话：「提取完把对应的东西去掉就换一台设备」的正解）：
//  1. 旧槽目录改名成 `<slotDir>.bak-<ts>`（不删，随时可回滚）；
//  2. 账号换一个新 SlotID → SlotDir 回填新值 → DeviceGUID 由新 SlotID 重新
//     派生（UUIDv5）。**注意**：只清空 SlotDir/DeviceGUID 是不够的——
//     DeviceGUID 是 `uuidv5(namespace, SlotID)` 的确定性派生，SlotID 不变就
//     永远算出同一个 GUID，等于"换了个目录但还是同一台设备"。
//  3. 保留 ID / Aliases / Cookies / 签到历史：这是"换设备"，不是"删账号"。
//
// 调用方（HTTP handler）必须要求 confirm=true 才调用本方法；本方法不做 UI
// 校验，也不读请求体。
func (p *Panel) codeartsSlotForget(a *codearts.Account) error {
	if a == nil || strings.TrimSpace(a.ID) == "" {
		return errors.New("账号为空，无法重置身份槽")
	}
	id := a.ID
	unlock := acquireCodeartsSlotLock(codeartsSlotLockKey(a))
	defer unlock()

	// ① 以号池里的**最新**记录为准确认账号还在、旧槽目录是哪个。
	pool, err := codearts.LoadPool()
	if err != nil {
		return fmt.Errorf("读号池失败: %w", err)
	}
	current := codeartsFindAccount(pool, id)
	if current == nil {
		return fmt.Errorf("账号已不在号池: %s", id)
	}
	current.EnsureSlot()
	oldDir := filepath.Join(codearts.SlotRoot(), current.SlotDir)
	if abs, aerr := filepath.Abs(oldDir); aerr == nil {
		oldDir = abs
	}

	// ② 旧槽先备份（浏览器还占着目录时 rename 会失败 → 明确报错，不硬删）。
	backup := ""
	if codeartsDirExists(oldDir) {
		backup, err = codeartsSlotArchiveDir(oldDir)
		if err != nil {
			return fmt.Errorf("备份旧槽目录失败（浏览器可能还占着该目录）: %w", err)
		}
		log.Printf("[codearts] 账号 %s 的旧槽已备份: %s -> %s", id, oldDir, backup)
	}

	// ③ 号池里换新槽身份。
	var updated *codearts.Account
	err = codearts.UpdatePool(func(latest *codearts.Pool) error {
		x := codeartsFindAccount(latest, id)
		if x == nil {
			return fmt.Errorf("账号已不在号池: %s", id)
		}
		x.EnsureSlot()
		x.SlotID = codeartsFreshSlotID(latest, id, x.SlotID)
		x.SlotDir = ""    // 由 EnsureSlot 回填成新 SlotID
		x.DeviceGUID = "" // 由新 SlotID 重新派生
		x.EnsureSlot()
		updated = x
		return nil
	})
	if err != nil {
		// 号池没改成功 → 把备份改回原名，保持"号池指向的目录仍然存在"。
		if backup != "" {
			if rbErr := os.Rename(backup, oldDir); rbErr != nil {
				log.Printf("[codearts] 账号 %s 重置失败且旧槽回滚失败（旧槽仍在 %s）: %v",
					id, backup, rbErr)
			}
		}
		return err
	}
	if updated == nil {
		return fmt.Errorf("账号 %s 重置后未取到新槽", id)
	}

	// ④ 让调用方手上的副本也看到新槽（只同步槽字段，不动其它字段）。
	a.SlotID, a.SlotDir, a.DeviceGUID = updated.SlotID, updated.SlotDir, updated.DeviceGUID
	log.Printf("[codearts] 账号 %s 身份槽已重置: slot_id=%s device_guid=%s",
		id, a.SlotID, a.DeviceGUID)
	return nil
}

// codeartsSlotArchiveDir 把槽目录改名成 `<dir>.bak-<ts>`，返回新路径。
//
// 目录不存在时返回 ("", nil)：调用方按"没有旧槽可备份"处理，不是错误。
func codeartsSlotArchiveDir(dir string) (string, error) {
	if !codeartsDirExists(dir) {
		return "", nil
	}
	bak := dir + ".bak-" + time.Now().Format("20060102-150405")
	if _, err := os.Stat(bak); err == nil {
		// 同一秒内重复重置：用纳秒 base36 后缀保证唯一。
		bak = dir + ".bak-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if err := os.Rename(dir, bak); err != nil {
		return "", err
	}
	return bak, nil
}

// codeartsFreshSlotID 为重置后的账号派生一个**池内唯一**的新 SlotID。
//
// 新 ID = 旧 ID + "-r" + 纳秒 base36：既能立刻换出不同的 DeviceGUID，又保持
// "这是同一个账号的重置槽"可读。用纳秒而不是固定序号，是为了同一账号在同
// 一秒内连续重置两次也不会算出同一个 GUID。
func codeartsFreshSlotID(pool *codearts.Pool, id, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = id
	}
	for i := 0; i < 8; i++ {
		cand := base + "-r" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if i > 0 {
			cand += "-" + strconv.Itoa(i)
		}
		if !codeartsSlotIDTaken(pool, id, cand) {
			return cand
		}
	}
	return base + "-r" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// codeartsSlotIDTaken 新 SlotID/目录名是否已被**别的**账号占用。
func codeartsSlotIDTaken(pool *codearts.Pool, selfID, candidate string) bool {
	if pool == nil {
		return false
	}
	for _, o := range pool.Accounts {
		if o == nil || o.ID == selfID {
			continue
		}
		o.EnsureSlot()
		if o.SlotID == candidate || o.SlotDir == candidate {
			return true
		}
	}
	return false
}

// ── 登录 / 补采的槽内执行 ────────────────────────────────────────────────

// codeartsChromeArgs 在**指定 profile 目录**内启动 Chromium 的参数。
//
// 抽成纯函数是为了让单测直接断言「登录窗口与补采的无头浏览器都带
// --user-data-dir=<该账号的槽目录>」——这是 §H.I2 的硬约束，不能只靠人眼
// 看 exec.Command 的实参。
func codeartsChromeArgs(port int, profileDir, startURL string, headless bool) []string {
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir=" + profileDir,
		"--no-first-run", "--no-default-browser-check",
	}
	if headless {
		args = append(args, "--headless=new", "--disable-gpu", "--window-size=1600,1000")
	}
	if startURL != "" {
		args = append(args, startURL)
	}
	return args
}

// codeartsLoginProfileDir 选"这次登录窗口"用的 profile 目录。
//
//   - 指定了账号（重登某个已知号）→ 直接用**该号自己的槽**：重登同一个号必须
//     是同一台设备，换槽等于凭空多出一台"新设备"（§H.5 阶段 3 的反面）；
//   - 没指定（"添加账号"，此刻还不知道是谁）→ 用槽根下的 pending-<ts> 占位
//     目录，登录成功后再由 codeartsAdoptPendingProfile 收编进该号的槽。
//
// 返回 inSlot=true 表示目录已经是长期槽，关闭窗口时**绝不能**删除。
func (p *Panel) codeartsLoginProfileDir(accountID string) (dir string, inSlot bool, err error) {
	id := strings.TrimSpace(accountID)
	if id == "" {
		return codeartsPendingDir(p), false, nil
	}
	pool, err := codearts.LoadPool()
	if err != nil {
		return "", false, fmt.Errorf("读号池失败: %w", err)
	}
	acc := codeartsFindAccount(pool, id)
	if acc == nil {
		return "", false, fmt.Errorf("账号不存在: %s", id)
	}
	dir, derr := p.codeartsSlotDir(acc)
	if derr != nil {
		return "", false, derr
	}
	return dir, true, nil
}

// codeartsAdoptPendingProfile 把"登录窗口"产生的 profile 收编进该账号的槽。
//
// 为什么是"搬"而不是"关掉就删"：profile 里的 Local State / Network Persistent
// State / Local Storage 就是这台设备的身份载体（§H.4）。首次登录时这个浏览器
// 实例已经生成了自己的设备身份，把它搬进槽，槽才是"登录时那台设备"；丢掉它
// 再建一个空槽，等于每次登录都换一次设备（风控异常信号）。
//
// 例外：账号**已有**设备身份的槽（老号重登但没带 id、走了 pending）不能被
// 本次登录覆盖 —— 那等于每次重登都把该号的设备换一次。此时保留原槽，
// 只把 pending 临时目录清掉（会话本身已经写进号池 Cookie，不受影响）。
func (p *Panel) codeartsAdoptPendingProfile(a *codearts.Account, pendingDir string) error {
	if a == nil || pendingDir == "" {
		return nil
	}
	target, err := p.codeartsSlotDir(a)
	if err != nil {
		return err
	}
	if !codeartsDirExists(pendingDir) || samePath(pendingDir, target) {
		return nil
	}
	unlock := acquireCodeartsSlotLock(codeartsSlotLockKey(a))
	defer unlock()
	if codeartsSlotHasDeviceIdentity(target) {
		log.Printf("[codearts] 账号 %s 的槽已有设备身份，保留原槽、丢弃本次 pending profile",
			a.ID)
		return os.RemoveAll(pendingDir)
	}
	if err := codeartsMergeMoveDir(pendingDir, target); err != nil {
		return fmt.Errorf("收编登录 profile %s -> %s 失败: %w", pendingDir, target, err)
	}
	return nil
}

// codeartsSlotHasDeviceIdentity 该 profile 目录里是否已经有 Chromium 生成的
// 设备身份载体（Local State / Network Persistent State）。
//
// 首次建槽时目录是空的，Chromium 启动后才生成；这两个文件存在就说明
// "这台设备已经上过线"，不能再被另一份 profile 覆盖。
func codeartsSlotHasDeviceIdentity(profileDir string) bool {
	for _, rel := range []string{
		"Local State",
		filepath.Join("Network", "Network Persistent State"),
	} {
		if info, err := os.Stat(filepath.Join(profileDir, rel)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// codeartsMergeMoveDir 把 src 的内容并入 dst。
//
// 同卷走 os.Rename（快）；跨卷 / 目标已存在 / 被占用时回退 copyDir（慢一次，
// 但保证设备身份不丢）。成功返回时 src 已被清空。
func codeartsMergeMoveDir(src, dst string) error {
	if err := os.MkdirAll(dst, codeartsSlotDirMode); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	moveFailed := false
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		if rerr := os.Rename(from, to); rerr != nil {
			moveFailed = true
			break
		}
	}
	if moveFailed {
		if cerr := copyDir(src, dst); cerr != nil {
			return cerr
		}
	}
	return os.RemoveAll(src)
}

// ── 小工具 ──────────────────────────────────────────────────────────────

// codeartsDirExists 路径存在且是目录。
func codeartsDirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// samePath 两个路径是否指向同一位置（Windows 不区分大小写）。
func samePath(left, right string) bool {
	la, lerr := filepath.Abs(left)
	ra, rerr := filepath.Abs(right)
	if lerr != nil || rerr != nil {
		return left == right
	}
	return strings.EqualFold(filepath.Clean(la), filepath.Clean(ra))
}
