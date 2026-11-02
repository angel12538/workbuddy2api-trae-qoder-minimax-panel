# A2 — baxia 点选验证逆向分析

- 日期: 2026-10-20
- 状态: 静态分析完成，动态验证待弹窗

## 1. 验证形态【已证实】

第二轮风控 = 阿里 baxia `captchaclick`（网格点选），主人描述："选择背包上有蜜蜂图案的"。
每次题目随机（`ques` 明文下发）。

## 2. 接口链【已证实 — 抓包】

```
GET /api/v1/users/_____tmd_____/punish?x5secdata=<...>&x5step=2&action=captchaclick
GET /api/v1/users/_____tmd_____/punishTextFetch?x5secdata=<...>     → 题库文案（明文 JSON）
GET /api/v1/users/_____tmd_____/gridClickGet?token=<32hex>&appKey=X82Y__<32hex>&ua=<bx-ua>
                                                                    → {encryptToken, imageData, ques, images[]}
GET /api/v1/users/_____tmd_____/gridClickReplace?token=...          → 换图
GET /api/v1/users/_____tmd_____/gridClickVerify?token=...           → 提交答案
GET /api/v1/users/_____tmd_____/report?x5secdata=...                → 行为上报
```

固定参数（**appKey 是硬编码常量**）：
```
appKey = X82Y__2493f13a1186b7864646f99765a13b57
```

## 3. 数据字段语义【已证实 — JS 源码】

`gridClickGet` 响应 `data` 结构（从 index.js 消费点反推）：
```js
{
  encryptToken: "<hex>",        // 加密验证凭据（提交时回传）
  ques: "<明文题目文字>",        // 如 "请选择包含蜜蜂的图片"
  imageData: "<加密图片>",       // 赋给 img.src（需 WASM 解密）
  images: [{ index, imageId, content }],   // 网格小图
  questionImage: "<加密示例图>",  // 部分题型有
  questionText: "<示例文字>"
}
```

**关键消费点**（index.js 原文）：
```js
a.options.SecCaptcha.updateImg(canvas, { token: c.data.encryptToken, data: c.data.imageData })
```
→ `SecCaptcha` 是 WASM 提供的解密/绘制模块，`updateImg` 是**解密入口**。

## 4. WASM 资产【已下载】

| 文件 | 大小 | 说明 |
|---|---|---|
| `sd/punish/0.0.4/program.wasm` | 21,040 B | 导出 10 项：`e`(mem) `f` `g` `h` `i` `j` `k`(table) `l` `m` `n` |
| `secdev/sec-captcha/0.0.20/program.wasm` | 30,621 B | gzip 压缩，需解压后分析 |
| `bsop-static/scratch-captcha/0.0.59/index.js` | 925 KB | 点选 UI 主逻辑（含 gridClick 流程）|
| `bsop-static/sufei-punish/0.1.129/build/punishpage.min.js` | 138 KB | punish 页面框架 |

## 5. 突破口（推荐路线）

**最高性价比：不逆向 WASM，直接读 canvas。**

理由：
1. WASM 解密后必须把图片画到 `<canvas>`（`SecCaptcha.updateImg(canvas, ...)`）
2. 同源 iframe 内的 canvas 可通过 `getImageData` 直接读像素（比 WASM 逆向成本低一个数量级）
3. 拿到明文图片后，用视觉模型识别目标物（如"背包上有蜜蜂图案"），算出网格坐标
4. 点选 + 提交 `gridClickVerify`，全程在浏览器内完成

**备选：WASM 导出函数分析**（`f/g/h/i/j/l/m/n`）——需先确定哪个是 decrypt，
可通过 hook `SecCaptcha.updateImg` 传参 + 返回值对比确定。

## 6. 待动态验证（需弹窗）

- [ ] punish iframe 的 DOM 结构（canvas 尺寸、网格数）
- [ ] `SecCaptcha` 在 iframe 内是否可访问
- [ ] 解密后 canvas 是否可 `getImageData`（是否被 tainted）
- [ ] `gridClickVerify` 的答案提交格式（坐标？索引？）
- [ ] 是否存在轨迹/时间校验
