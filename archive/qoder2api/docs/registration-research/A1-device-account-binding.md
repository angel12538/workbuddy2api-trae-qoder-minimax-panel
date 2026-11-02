# A1 归档 — 设备↔账号绑定机制（证据已收，待深挖）

- 日期: 2026-10-20
- 状态: **归档**（证据充分，暂不深挖；A2 优先）

## 铁证

App 登录流程自动打开的页面：

```
URL   : https://qoder.com/device/selectAccounts?nonce=<可空>
标题  : 客户端身份验证 | Qoder - 智能体编程平台
内容  : 允许登录
        继续使用此账号吗？
        账号: Wei Chen
        [继 续]  [使用其他账号登录]
```

## 结论
1. Qoder 服务端维护 **device → accounts[]** 关联表（否则无法列出"该设备用过哪些账号"）
2. 这是标准 OAuth Device Authorization Grant 的账号选择步骤
3. 与 H-App-Revoke 假设兼容：网页注册给 300 积分/Pro Trial（账号级），
   App 登录按设备查历史 → 判定"同设备多账号" → 撤销权益

## 待办（未来续做）
- [ ] 确认 deviceId 来源：cosy machineid(=f(serial))？浏览器指纹？nonce 绑定？
      方法：抓 App 登录时的 /device/selectAccounts 请求参数 + 对比本机 machineid
- [ ] "使用其他账号登录"分支行为
- [ ] 权益撤销的触发接口（登录后哪个调用把 userplan 改成 FREE）
- [ ] 决定性实验：**全新设备**登录新账号 → 是否保留 300 积分（验证 H-App-Revoke）

## 附带发现
- `window.csrfToken = '_echo_csrf_using_sec_fetch_site_'`
  → Qoder 用 `Sec-Fetch-Site` 做 CSRF 防御；curl 需带 `Sec-Fetch-Site: same-origin` 才能过
- 页面注入的全局配置（可用于协议还原）:
  ```
  window.captchaScene = {"create_me_verification_codes":"udwgwv7k2",
                          "create_sessions":"17on5mo7v",
                          "create_verification_codes":"1r7eif79x"}
  window.qmindNotebookEnabled / commerceEnabled / invitationEnabled / teamsSellingEnabled ...
  window.githubAppClientID = 'Iv23lie3f4AhSxKXzxdE'
  window.supabaseAppClientID = '1f2c727a-4240-4111-8247-37b366034c33'
  ```
