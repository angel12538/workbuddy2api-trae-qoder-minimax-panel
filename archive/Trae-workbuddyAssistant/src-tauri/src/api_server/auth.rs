use std::sync::Arc;

use axum::extract::{Request, State};
use axum::http::StatusCode;
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use sha2::{Digest, Sha256};

use super::ApiSharedState;

/// Bearer Token 鉴权中间件：
/// - /health 跳过鉴权
/// - api_key 为空时跳过鉴权
/// - 否则校验 Authorization: Bearer <key>
pub async fn bearer_auth(
    State(state): State<Arc<ApiSharedState>>,
    request: Request,
    next: Next,
) -> Response {
    if request.uri().path() == "/health" {
        return next.run(request).await;
    }
    if state.api_key.is_empty() {
        return next.run(request).await;
    }
    let authz = request
        .headers()
        .get("authorization")
        .and_then(|v| v.to_str().ok());
    match authz {
        Some(s) if s.len() > 7 && s[..7].eq_ignore_ascii_case("Bearer ") => {
            let key = &s[7..];
            // 常量时间比较（requirements-v2.md §鉴权要求「常量时间比较，防时序攻击」）。
            // 直接 `key == state.api_key` 是逐字节短路比较（memcmp），命中前缀即返回，
            // 响应时间可用来逐字节猜出 api_key。
            //
            // 做法与本项目的 Go 网关（internal/httpauth）一致：先对两侧各取 SHA-256
            // 摘要，再对**定长**摘要做无短路比较——比较次数与内容无关，耗时恒定；
            // 长度差异也不会提前暴露（摘要恒为 32 字节）。
            if constant_time_eq(&sha256(key), &sha256(&state.api_key)) {
                next.run(request).await
            } else {
                (StatusCode::UNAUTHORIZED, "invalid api key").into_response()
            }
        }
        _ => (StatusCode::UNAUTHORIZED, "missing api key").into_response(),
    }
}

/// sha256 取定长 32 字节摘要。
fn sha256(data: &str) -> [u8; 32] {
    let mut hasher = Sha256::new();
    hasher.update(data.as_bytes());
    let out = hasher.finalize();
    let mut buf = [0u8; 32];
    buf.copy_from_slice(&out);
    buf
}

/// constant_time_eq 定长字节的常量时间比较。
///
/// 不做短路：所有字节都要比较完，差异累加到 diff，最后一次性判定。
/// 两侧长度恒等（都是 32 字节摘要），不存在长度分支带来的时间差。
fn constant_time_eq(a: &[u8; 32], b: &[u8; 32]) -> bool {
    let mut diff = 0u8;
    for i in 0..32 {
        diff |= a[i] ^ b[i];
    }
    diff == 0
}
