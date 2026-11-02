use std::io::{BufRead, BufReader, Write};
use tauri::{AppHandle, Emitter, State};

use serde::Deserialize;
use crate::fs_utils;
use crate::state::AppState;
use crate::commands::accounts::{build_account_views, resolve_user_ids};
use crate::python::spawn_script;

#[derive(Deserialize)]
pub struct CheckinOpts {
    pub scope: String,
    #[serde(default)]
    pub user_ids: Option<Vec<String>>,
    #[serde(default)]
    pub skip_checked_in: bool,
    #[serde(default)]
    pub skip_expired: bool,
}

#[tauri::command]
pub fn checkin_start(
    app: AppHandle,
    state: State<AppState>,
    opts: CheckinOpts,
) -> Result<(), String> {
    fs_utils::app_log(
        &state.data_dir,
        &format!(
            "checkin_start 已到达 Rust: python_dir={:?}, python_exe={}, auto_checkin.py 存在={}",
            state.python_dir,
            state.python_exe,
            state.python_dir.join("auto_checkin.py").exists()
        ),
    );
    let mut uids = resolve_user_ids(&state, &opts.scope, opts.user_ids)?;
    if opts.skip_checked_in || opts.skip_expired {
        let views = build_account_views(&state);
        uids.retain(|u| {
            let v = views.iter().find(|a| &a.user_id == u);
            let mut keep = true;
            if let Some(v) = v {
                if opts.skip_checked_in && v.checked_today == Some(true) {
                    keep = false;
                }
                if opts.skip_expired
                    && (v.jwt_exp_hours.is_none() || v.jwt_exp_hours.unwrap() <= 0.0)
                {
                    keep = false;
                }
            }
            keep
        });
    }
    // 过滤冷却中的账号（SessionDead 永久跳过，其他类型冷却中跳过）
    {
        let views = build_account_views(&state);
        let cooled: std::collections::HashSet<&str> = views
            .iter()
            .filter(|v| v.cooldown_type.is_some())
            .map(|v| v.user_id.as_str())
            .collect();
        let before = uids.len();
        uids.retain(|u| !cooled.contains(u.as_str()));
        let skipped = before - uids.len();
        if skipped > 0 {
            crate::fs_utils::app_log(
                &state.data_dir,
                &format!("跳过 {} 个冷却中账号", skipped),
            );
        }
        // 积分过期感知调度：按 credits_expire_at 升序排列（最近过期的优先签到）
        // 无过期时间的账号排在最后；过期时间相同的按剩余积分降序
        let rc: crate::models::RemainingCreditsFile =
            crate::fs_utils::read_json(&state.path("remaining_credits.json"));
        uids.sort_by(|a, b| {
            let ea = rc.expire_times.get(a).copied();
            let eb = rc.expire_times.get(b).copied();
            match (ea, eb) {
                (Some(ta), Some(tb)) => {
                    if ta == tb {
                        // 过期时间相同 → 剩余积分降序
                        let ca = rc.credits.get(a).copied().unwrap_or(0.0);
                        let cb = rc.credits.get(b).copied().unwrap_or(0.0);
                        cb.partial_cmp(&ca).unwrap_or(std::cmp::Ordering::Equal)
                    } else {
                        ta.cmp(&tb)
                    }
                }
                (Some(_), None) => std::cmp::Ordering::Less,
                (None, Some(_)) => std::cmp::Ordering::Greater,
                (None, None) => std::cmp::Ordering::Equal,
            }
        });
    }
    let accounts_arg = uids.join(",");
    let retry = state.settings().retry.max(0) as u32;
    let mut args = vec!["--json-stream".to_string()];
    if !accounts_arg.is_empty() {
        args.push("--accounts".to_string());
        args.push(accounts_arg);
    }
    if retry > 0 {
        args.push("--retry".to_string());
        args.push(retry.to_string());
    }

    let mut child = spawn_script(&state, "auto_checkin.py", &args, true)?;
    let stdout = child.stdout.take().ok_or("无法获取子进程输出")?;
    let stderr = child.stderr.take();
    let data_dir = state.data_dir.clone();
    let app2 = app.clone();

    crate::fs_utils::app_log(&state.data_dir, &format!("签到已启动: {} 个账号", uids.len()));

    // stdout 线程：NDJSON 解析 -> 事件 emit + 日志追加
    //
    // 兜底：脚本若在发出 done 之前崩溃（导入失败、依赖缺失、Python 异常），
    // 前端只监听 checkin-progress，没有 done 就永远不会把「运行中」置回 false，
    // 界面会永久卡住。这里记录是否已见 done，退出时若未见则补发一个合成 done。
    let saw_done = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
    let accounts_seen = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let saw_done2 = saw_done.clone();
    let accounts_seen2 = accounts_seen.clone();
    std::thread::spawn(move || {
        let reader = BufReader::new(stdout);
        let log_path = data_dir.join("logs").join("checkin.log");
        if let Some(p) = log_path.parent() {
            let _ = std::fs::create_dir_all(p);
        }
        for line in reader.lines() {
            if let Ok(l) = line {
                let l = l.trim().to_string();
                if l.is_empty() {
                    continue;
                }
                if let Ok(v) = serde_json::from_str::<serde_json::Value>(&l) {
                    match v.get("type").and_then(|t| t.as_str()) {
                        Some("done") => {
                            saw_done2.store(true, std::sync::atomic::Ordering::Relaxed);
                        }
                        Some("account") => {
                            accounts_seen2.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                        }
                        _ => {}
                    }
                    let _ = app2.emit("checkin-progress", &v);
                    if v.get("type").and_then(|t| t.as_str()) == Some("done") {
                        let _ = app2.emit("checkin-done", &v);
                    }
                }
                if let Ok(mut f) = std::fs::OpenOptions::new()
                    .create(true)
                    .append(true)
                    .open(&log_path)
                {
                    let _ = writeln!(f, "[{}] {}", crate::fs_utils::now_ts(), l);
                }
            }
        }
        // 等待子进程退出，并兜底收尾。
        let status = child.wait();
        if !saw_done.load(std::sync::atomic::Ordering::Relaxed) {
            // 脚本没走到 done 就退出了——多半是异常/崩溃。补一个 done，
            // 让前端把「运行中」收掉，并如实报告失败数。
            let seen = accounts_seen.load(std::sync::atomic::Ordering::Relaxed) as u64;
            let code = match &status {
                Ok(st) => st.code().unwrap_or(-1),
                Err(_) => -1,
            };
            let msg = format!(
                "签到脚本异常退出（exit={}），已处理 {} 个账号",
                code, seen
            );
            crate::fs_utils::app_log(&data_dir, &msg);
            let synthetic = serde_json::json!({
                "type": "done",
                "ok": 0,
                "already": 0,
                "failed": seen,
                "total": seen,
                "error": format!("签到脚本异常退出（exit={}）", code),
            });
            let _ = app2.emit("checkin-progress", &synthetic);
            let _ = app2.emit("checkin-done", &synthetic);
        }
    });

    // stderr 线程：防止管道缓冲区写满导致子进程死锁
    if let Some(stderr) = stderr {
        let data_dir2 = state.data_dir.clone();
        std::thread::spawn(move || {
            let log_path = data_dir2.join("logs").join("checkin.log");
            let reader = BufReader::new(stderr);
            for line in reader.lines() {
                if let Ok(l) = line {
                    let l = format!("[stderr] {}", l.trim());
                    if let Ok(mut f) = std::fs::OpenOptions::new()
                        .create(true)
                        .append(true)
                        .open(&log_path)
                    {
                        let _ = writeln!(f, "[{}] {}", crate::fs_utils::now_ts(), l);
                    }
                }
            }
        });
    }

    Ok(())
}
