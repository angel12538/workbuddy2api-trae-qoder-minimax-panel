//! license_guard —— 本 fork 的**无授权校验**实现。
//!
//! ## 为什么是这样
//!
//! 原实现（上游 cxqc168-wq/Trae-workbuddyAssistant）是作者的**专有授权门**：
//! 激活口令 → 远程验证服务器换签名凭证 → 内置公钥验签 → 机器码绑定 → 到期检查。
//! 该实现与其验证服务器（`http://154.222.24.81:8443`）**不随仓库开源**，也
//! 不属于本 fork 的分发内容。
//!
//! 本 fork 保留**命令接口不变**（`license_status` / `license_activate` 仍注册、
//! 前端 `ActivationGate` 仍可编译），但校验一律放行：
//!
//! - `check_license()` 恒返回 `Ok(STATUS_OK)` → 前端永远不进入激活门；
//! - `activate()` 返回明确错误，而不是静默成功。
//!
//! 这样 fork 的行为与仓库根目录的 **MIT 协议**一致（协议授予无限制使用与修改权，
//! 与「必须联网激活」自相矛盾），同时不触碰、不分发作者的授权实现。
//!
//! 如果你要的是**带激活门的官方发行版**，请用上游仓库：
//! <https://github.com/cxqc168-wq/Trae-workbuddyAssistant>
//!
//! 原实现（保留原样参考）：`mod.rs` / `public_key.pem` ——
//! 激活协议：POST {SERVER_URL}/api/activate {code, machine_id, nonce}
//! → {"payload_b64": canonical JSON 的 Base64, "signature_b64": RSA-SHA256 签名的 Base64}
//! 凭证：%USERPROFILE%\.license_guard\license.dat = {payload_b64, signature_b64, last_seen}
//! 校验：内置公钥验签 → machine_id 比对 → expires_at 检查 → last_seen 时钟回拨检查

/// 验证服务器地址（上游专有，本 fork 不使用）。
pub const SERVER_URL: &str = "http://154.222.24.81:8443";

/// 内置公钥（上游专有，不随本 fork 分发）。
pub const PUBLIC_KEY_PEM: &str = "";

// 授权状态常量（与上游取值一致，前端 message_for 按这些值映射文案）。
pub const STATUS_OK: &str = "ok";
pub const STATUS_MISSING: &str = "missing";
pub const STATUS_CORRUPTED: &str = "corrupted";
pub const STATUS_SIGNATURE_INVALID: &str = "signature_invalid";
pub const STATUS_MACHINE_MISMATCH: &str = "machine_mismatch";
pub const STATUS_EXPIRED: &str = "expired";
pub const STATUS_CLOCK_ROLLBACK: &str = "clock_rollback";
pub const STATUS_FINGERPRINT_ERROR: &str = "fingerprint_error";

/// 采集机器指纹（本 fork 不做机器绑定，返回空串）。
pub fn collect_machine_id() -> Result<String, ()> {
    Ok(String::new())
}

/// 保存授权凭证（本 fork 无授权模块，恒失败）。
pub fn save_license(payload_b64: &str, signature_b64: &str) -> std::io::Result<()> {
    let _ = (payload_b64, signature_b64);
    Err(std::io::Error::new(
        std::io::ErrorKind::Unsupported,
        "本 fork 不含授权模块（MIT 协议，无需激活）",
    ))
}

/// 校验本地授权（本 fork 恒放行）。
pub fn check_license() -> Result<&'static str, ()> {
    Ok(STATUS_OK)
}

/// 提交激活口令（本 fork 无授权模块，恒失败并给出明确说明）。
pub fn activate(code: &str) -> Result<(String, String), String> {
    let _ = code;
    Err("本 fork 不含授权模块（MIT 协议，无需激活）。需要带激活门的版本请用上游仓库：https://github.com/cxqc168-wq/Trae-workbuddyAssistant".to_string())
}
