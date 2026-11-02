//! 进程内共享的并发小工具。
//!
//! ## safe_lock：毒化锁（poisoned mutex）容错
//!
//! `Mutex::lock().unwrap()` 在持锁线程 panic 时会把锁标记为「毒化」，此后每一次
//! `lock().unwrap()` 都会再次 panic——一次偶发 panic 沿调用链扩散成持续崩溃。
//! `safe_lock` 在拿到 `PoisonError` 时取回其内部数据（`into_inner`）继续执行：
//! 数据可能只写了一部分，但**应用还能继续跑**，这比整条链崩掉更可取。
//!
//! API 服务模块此前已各自持有一份同样的实现（commands/api_server.rs 与
//! api_server/routes.rs），workbuddy 模块却一直用裸 `lock().unwrap()`——两种风格
//! 并存，同一把锁的处置方式取决于调用点在哪个模块，极易踩坑。这里收敛成一处。

use std::sync::{Mutex, MutexGuard};

/// 安全获取 Mutex 锁：锁被毒化（panic 导致）时恢复内部数据继续运行。
///
/// 用法与 `Mutex::lock()` 相同，返回 `MutexGuard`。
pub fn safe_lock<'a, T>(m: &'a Mutex<T>) -> MutexGuard<'a, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}
