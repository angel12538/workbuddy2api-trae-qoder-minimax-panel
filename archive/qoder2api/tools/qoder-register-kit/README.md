# qoder-register-kit — Qoder 注册链路研究工具包

> 从 2026-10-20 的 Qoder 注册链路逆向中沉淀出的可复用工具。
> **仅用于协议研究**，不建议用于生产环境的批量注册。

---

## 快速导航

- [工具清单（含用途详解）](#工具清单含用途详解)
- [依赖与前置](#依赖与前置)
- [典型工作流](#典型工作流)
- [★ 关键经验（踩过的坑）](#-关键经验踩过的坑)
- [协助者注意事项](#协助者注意事项)

---

## 工具清单（含用途详解）

### 核心库（`tools/qoder-register-kit/*`）

#### `cdp.js` — CDP 驱动（人类节奏）

**用途**：封装 Chrome DevTools Protocol，提供"像人一样"操作浏览器的基础能力。
所有流程脚本都基于它，是工具包的地基。

**解决什么问题**：直接用 CDP 发原始事件太快、太规则，容易被风控判定为机器人；
手工写 `Input.dispatchMouseEvent` 又要处理几十行粘合代码。

**提供的能力**：

| 方法 | 说明 |
|---|---|
| `new CDP(wsUrl 或 ws 实例, sessionId?)` | 连接；传已有 ws 实例可复用连接（session 绑定连接，必须复用）|
| `c.eval(expression, awaitPromise?)` | 执行 JS 并取值（内部处理 `Runtime.evaluate` 细节）|
| `c.send(method, params)` | 原始 CDP 调用（带 15s 超时保护）|
| `c._fire(method, params)` | 发送后不等回包（批量键盘/鼠标事件提速）|
| `CDP.sleep(min, max)` | 区间随机延迟 |
| `c.humanMove(x1,y1,x2,y2)` | 贝塞尔曲线轨迹移动鼠标（带缓动 + 1.5px 抖动）|
| `c.humanClick(x, y)` | 先移动再按下/抬起（含随机停顿）|
| `c.humanType(sel, text)` | 聚焦 → 原生 setter 清空 → 逐字符输入（含偶发"思考停顿"）|
| `c.box(sel)` | 取元素中心坐标（相对视口）|

**关键设计**：
- 内部用 `WeakMap` 按 ws 实例共享消息 ID 计数器 —— 否则同一连接上两个 CDP 实例会 ID 冲突
- `humanType` 的清空走"原生 setter + 重置 `_valueTracker`"，这是 React 受控组件唯一可靠的清空方式

---

#### `incog.js` — 无痕上下文管理

**用途**：创建一个**独立的无痕 browserContext**，在其中开 target 并导航。

**解决什么问题**（这是本项目最重要的坑之一）：

1. **登录态干扰**：如果直接在你已登录 Qoder 的 Chrome tab 里打开 `users/sign-up`，
   会被立即重定向到 `/account/profile`，根本看不到注册表单。
2. **上下文污染**：一次失败的注册尝试会污染该 tab 的 React 状态与 baxia 状态，
   之后**表单提交会静默失效**（点"继续"毫无反应，也不报错）。
   唯一解法是新开一个干净的 browserContext。

**输出**：打印 `browserContextId` / `targetId` / `sessionId`，并写入 `.incog.json`
供后续脚本读取 targetId。

**用法**：
```bash
rm -f .incog.json && node incog.js    # 每次失败后重跑，拿一个全新环境
```

---

#### `mailbox.py` — 临时邮箱客户端

**用途**：对接自建的 Cloudflare Worker 临时邮箱服务，支持**建地址 → 收信 → 提取验证码**，
用于注册流程的邮箱验证环节。

**API 契约**（实测）：

```
Admin（header: x-admin-auth: <admin passkey>）
  POST /admin/new_address   {enablePrefix, name, domain}  → {jwt, address, address_id}
  GET  /admin/address?limit&offset
  GET  /admin/mails?limit&offset&address=<addr>

地址用户（header: Authorization: Bearer <address jwt>）
  GET  /api/mails?limit&offset      → {results, count}
  GET  /api/mail/<id>
```

**注意**：
- `domain` 参数必须是邮箱服务实际接受的域名（本项目为 `school-service.online`），
  传错会返回 `400 Invalid domain`
- `enablePrefix:true` 时地址会自动加 `tmp` 前缀
- **admin passkey 不要写进仓库**，用环境变量传入

**典型调用**（Python）：
```python
from mailbox import Mailbox
mb = Mailbox(base_url, admin_passkey)
jwt, addr = mb.new_address("qoder-01")
code = mb.wait_for_code(jwt, timeout=120)   # 轮询并从正文提取 6 位数字
```

---

#### `detect_gap.py` — 滑块缺口检测

**用途**：识别 AliyunCaptcha 滑块验证码的**缺口水平位置**，输出拖拽目标距离。

**输入**：两张 PNG（`bg.png` 296×200 背景图、`puzzle.png` 52×200 拼图块）
**输出**：JSON `{"bestX": 237, "W": 296, "PW": 52, "top5": [...]}`

**算法**：模板匹配 —— 把拼图块的**不透明像素**（alpha≥128）作为模板，
在背景图上逐列滑动，计算 RGB 差值和，取最小者为缺口位置。

**实测效果**：
- 多数题目能给出与视觉一致的候选（如 237、244）
- **局限**：评分曲线可能很平（多个相邻位置分数接近），存在 ±2~3px 误差；
  而阿里云验证码的容差较窄，因此**位置正确不等于一定通过**

**用法**：
```bash
python3 detect_gap.py bg.png puzzle.png
# 或从页面导出后直接跑
```

---

### 流程脚本（`flows/`，按执行顺序）

#### `page-submit.js` — 注册第 1 步

**用途**：填入「名字 / 姓 / 邮箱」→ 勾选条款 → 提交，推进到密码步。

**核心技巧**：antd Form **无法用 DOM 事件注入**。脚本从 `__reactFiber$` 向上遍历
找到 Form 实例，直接调 `setFieldsValue()`；事实校验用 `validateFields()` 验证。

**输入**：修改文件顶部 `EMAIL` 常量
**输出**：终端日志（是否进入密码步）

**为什么不能用 `input.value = x`**：React 的 value tracker 会认为值没变，
不触发 `onChange`，Form store 保持为空 → 提交时报"名字不能为空"。

---

#### `step2-submit.js` — 注册第 2 步

**用途**：填入密码 → 提交，触发第一轮 AliyunCaptcha 滑块。

**前提**：必须已跑完 `page-submit.js`（页面处于密码步）
**输出**：检测到 `#aliyunCaptcha-captcha-body` 宽度 > 50 时打印 `★★★ 滑块出现`

---

#### `probe-captcha.js` — 滑块结构观察

**用途**：只读导出 AliyunCaptcha 的 DOM 结构、iframe、scene 配置、截图。

**适合**：首次接触该站点时了解验证码形态
**产出**：`artifacts/captcha-round1.png` + 终端打印的 DOM JSON

---

#### `solve-slider6.js` — 滑块求解（闭环自适应）

**用途**：自动解 AliyunCaptcha 滑块 —— 检测缺口 → 计算拖拽距离 → 拟人拖拽 → 判定结果。

**算法（v6，最终版）**：

```
1. JS .click() 触发验证码（CDP 鼠标事件对该 SDK 无效）
2. 等图片与手柄就绪（轮询 DOM）
3. 取 #aliyunCaptcha-img / #aliyunCaptcha-puzzle 的 src → 下载
4. Python detect_gap.py 算缺口 x
5. 拖拽分三段，每段后等动画稳定再测实际位移，动态修正比例
6. 松手前再测一次拼图偏移 vs 缺口，误差 ≤1px 才松手
```

**关键数据**：
- 拼图位移 : 手柄位移 ≈ **0.91**（实测，非 1:1）
- 拖拽轨迹：三段式缓动 + y 轴 1.6px 摆动 + 7% 概率微停顿 + 过冲回拉

**已知限制**：位置算准仍可能被拒（baxia 行为评分）；
成功率不稳定，**建议先人工过一次验证完整链路**。

---

#### `type-code.js` — 填写邮箱验证码

**用途**：把 6 位验证码填入 `input[type=text]`（maxLength=6）。

**注意**：
- 该输入框是**受控组件**，必须走真实键盘事件；直接赋值无效
- 填满 6 位后页面**不会自动提交**，需调用组件的 `onChangeCallback`
  （从 React fiber 的 `memoizedProps` 上取）

**用法**：
```bash
node type-code.js 418251
```

---

#### `watch-baxia2.js` — 等待 baxia 弹窗

**用途**：轮询页面，检测 baxia punish iframe 是否出现（宽 > 100px 判定为已展示）。

**产出**：状态变化日志；检测到即退出（code 0）
**典型用途**：配合人工完成滑块后，自动接住 baxia 阶段

---

#### `analyze-baxia.js` — baxia 只读分析

**用途**：在 baxia 弹窗打开时，**只读**提取验证结构，不解题、不点击。

**产出**：
- iframe DOM 结构与尺寸
- 9 个网格 canvas（`click-grid-0..8`）的像素可读性检测
- 题目 canvas（`click-question-canvas`）信息
- `window._config_` / `_render_config_`（含 NCTOKENSTR、SECDATA、FORMACTIOIN、NCAPPKEY）
- 可选：导出题目图 + 9 张网格图为 PNG（`artifacts/baxia/`）

**关键发现（本工具验证的）**：iframe **同源**，canvas 未被跨域污染，
`getImageData()` / `toDataURL()` 可直接拿到 WASM 解密后的明文图片 ——
**无需逆向 WASM**。

---

#### `capture-success.js` — 全量网络监听存档

**用途**：监听并保存所有 `/api/`、captcha、punish 相关请求与响应到 JSONL。

**记录内容**：时间戳、方法、URL、请求头、POST body、状态码、响应体（截断 3KB）
**特别标注**：自动提取 `bx-ua` 指纹，命中注册成功响应时打 `★★` 标记

**产出**：`artifacts/manual-flow-<ISO时间>.jsonl`
**典型用途**：人工走一遍成功流程，用它抓完整样本（后续自动化的对照基准）

**用法**：
```bash
node capture-success.js ws://localhost:9366/devtools/page/<TARGET_ID> > /tmp/cap.log 2>&1 &
```

---

#### `net-observe.js` — 单次请求定位

**用途**：临时开 Network 监听，触发一次动作，**立刻看到请求打到了哪个 URL、body 长什么样**。

**典型用途**：不确定某个按钮调什么接口时，用它一次定位
（本项目就是靠它发现注册接口是 `/api/v1/users`）

---

## 依赖与前置

```bash
# Node 18+（原生 WebSocket），另需 ws 包
cd tools/qoder-register-kit && npm i ws

# Python
pip install pillow          # detect_gap.py 必需
pip install pytesseract     # 仅当你想试 OCR（实测对 baxia 无效）
```

**带调试端口的 Chrome**：
```bash
/Applications/Google\ Chrome.app/Contents/MacOS/Google\ Chrome \
  --remote-debugging-port=9366 \
  --user-data-dir="$HOME/.chrome-cdp-profile"
```
> Chrome 90+ 必须用独立 `--user-data-dir`，否则调试端口不生效。

---

## 典型工作流

```bash
# 0) 起一个干净的无痕环境
rm -f .incog.json && node incog.js
#    → 记下 targetId，填入各脚本顶部的 PAGE_WS

# 1) 填表 → 密码步
node flows/page-submit.js

# 2) 填密码 → 滑块出现
node flows/step2-submit.js

# 3A) 自动解滑块（成功率不稳定）
node flows/solve-slider6.js
# 3B) 或人工拖一次滑块，同时开着监听
node flows/capture-success.js ws://localhost:9366/devtools/page/<ID> &

# 4) 收验证码（Python）
python3 -c "..."   # 用 mailbox.py 取码

# 5) 填验证码 → 提交
node flows/type-code.js 418251

# 6) 若弹 baxia
node flows/watch-baxia2.js     # 等弹窗
node flows/analyze-baxia.js    # 只读分析 + 导出图片
```

---

## ★ 关键经验（踩过的坑）

| # | 坑 | 解法 |
|---|---|---|
| 1 | **antd Form 无法用 DOM 事件注入** | 从 `__reactFiber$` 向上找 Form 实例，调 `setFieldsValue` |
| 2 | **注册接口是 `/api/v1/users`** | 不是 `/users/sign-up`（后者只是前端路由）|
| 3 | **CSRF 用 `Sec-Fetch-Site` 校验** | `window.csrfToken = '_echo_csrf_using_sec_fetch_site_'`，curl 需带该头 |
| 4 | **无痕上下文会被污染** | 每次失败后新建 browserContext，否则提交静默失效 |
| 5 | **AliyunCaptcha 只认 JS `.click()`** | CDP 鼠标事件无法触发滑块组件 |
| 6 | **baxia iframe 同源可访问** | 无需逆向 WASM，`getImageData` 直读 canvas |
| 7 | **题目文字是 anti-OCR 像素图** | tesseract 全败（低对比度+混色+扭曲），需多模态视觉模型 |
| 8 | **拼图位移 ≠ 手柄位移** | 实测比例约 0.91，且读取位置要等动画稳定 |
| 9 | **CDP session 绑定连接** | 复用同一 WebSocket，ID 计数器要共享（WeakMap）|
| 10 | **网络分流** | qoder.com 需直连；邮件 API 需代理（视环境而定）|

---

## 协助者注意事项

1. **合规边界**
   - 本工具包面向**协议研究与学习**；请勿用于批量注册、薅羊毛、绕过风控等用途。
   - 请遵守目标站点的服务条款与当地法律法规。
2. **不要提交凭证**
   - `mailbox.py` 的 admin passkey、账号密码、`bx-ua` 指纹等**不得写入仓库**。
   - 抓包存档（`artifacts/*.jsonl`）提交前必须脱敏。
   - 脚本里的邮箱、密码统一用占位符（`target@example.com` / `CHANGE_ME_PASSWORD`）。
3. **保持人类节奏**
   - `cdp.js` 内置的随机延迟与拟人轨迹**请勿移除** —— 高频请求会触发风控，
     也会影响其他人的研究环境。
4. **可回滚**
   - 所有实验仅在浏览器无痕上下文内进行，不改动系统配置与硬件信息。
5. **提交前自检**
   ```bash
   git diff                   # 确认无凭证
   git status                 # 确认无 node_modules、无抓包原件
   ```
6. **欢迎贡献的方向**
   - **多模态识别**：题目文字（anti-OCR）+ 9 张网格图分类（如"找出所有轮胎"）
   - **`bx-ua` 指纹**：baxia SDK 的设备指纹生成算法
   - **`program.wasm`**：导出函数 `f/g/h/i/j/l/m/n` 的语义
   - **设备绑定**：`/device/selectAccounts` 的 deviceId 参数来源
