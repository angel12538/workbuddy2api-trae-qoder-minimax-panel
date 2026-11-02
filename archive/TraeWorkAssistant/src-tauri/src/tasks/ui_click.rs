//! UI 坐标点击（F-18 兜底，原 src-python/workbuddy_ui_click.py 的 Rust 移植）。
//! 无 API 可用时的最后手段：驱动鼠标对 WorkBuddy 客户端签到按钮做坐标点击。
//! 红线：仅手动触发、默认关闭（settings.ui_click_enabled）；坐标需用户预配置；
//! 全程零 token 输出；单次执行只点击一次，不做循环连点。
//! 注：windows-sys 的 mouse_event 已标注 deprecated（违反零 warning 红线），
//! 点击统一走等价的 SendInput（注入级 API，行为一致）。

use std::thread::sleep;
use std::time::Duration;

use windows_sys::Win32::Foundation::POINT;
use windows_sys::Win32::UI::Input::KeyboardAndMouse::{
    SendInput, INPUT, INPUT_MOUSE, INPUT_0, MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP, MOUSEINPUT,
};
use windows_sys::Win32::UI::WindowsAndMessaging::{
    GetCursorPos, GetSystemMetrics, SetCursorPos, SM_CXVIRTUALSCREEN, SM_CYVIRTUALSCREEN,
    SM_XVIRTUALSCREEN, SM_YVIRTUALSCREEN,
};

const MOVE_DELAY_MS: u64 = 350; // 移动后停顿，给悬浮反馈留时间
const CAPTURE_DELAY_S: u64 = 3; // 取点倒计时
const CLICK_GAP_MS: u64 = 50; // 按下与抬起间隔（对齐 python 版 0.05s）

/// 3 秒倒计时后记录当前鼠标坐标（取点模式，对齐 python --capture：
/// 用户把鼠标移到签到按钮上，倒计时结束时读位置）。
pub fn capture_point() -> Result<(i32, i32), String> {
    sleep(Duration::from_secs(CAPTURE_DELAY_S));
    let mut pt = POINT { x: 0, y: 0 };
    let ok = unsafe { GetCursorPos(&mut pt) };
    if ok == 0 {
        return Err("取点失败：GetCursorPos 不可用".to_string());
    }
    Ok((pt.x, pt.y))
}

/// 移动到坐标并左键单击一次（对齐 python --click）。
/// 审查修复（M11）：坐标校验从「<=0 一刀切拒绝」改为 Windows 虚拟屏幕边界校验——
/// 多显示器布局下主屏左侧/上方的副屏坐标为负（SM_XVIRTUALSCREEN 可为负），
/// 原校验会让左侧副屏永远不可用；越出虚拟屏幕范围仍拒绝，防误触。
pub fn click_at(x: i32, y: i32) -> Result<(), String> {
    // 虚拟屏幕边界（GetSystemMetrics 异常时四值为 0，边界退化为 [0,0) 空区间，
    // 任何坐标均判越界 → fail-closed，不注入）
    let vx = unsafe { GetSystemMetrics(SM_XVIRTUALSCREEN) };
    let vy = unsafe { GetSystemMetrics(SM_YVIRTUALSCREEN) };
    let vw = unsafe { GetSystemMetrics(SM_CXVIRTUALSCREEN) };
    let vh = unsafe { GetSystemMetrics(SM_CYVIRTUALSCREEN) };
    if x < vx || x >= vx + vw || y < vy || y >= vy + vh {
        return Err(format!(
            "坐标 ({x},{y}) 超出虚拟屏幕范围 [{vx},{vy}) x [{},{})",
            vx + vw,
            vy + vh
        ));
    }
    let moved = unsafe { SetCursorPos(x, y) };
    if moved == 0 {
        return Err("移动鼠标失败：SetCursorPos 返回 0".to_string());
    }
    sleep(Duration::from_millis(MOVE_DELAY_MS));
    send_mouse_flag(MOUSEEVENTF_LEFTDOWN)?;
    sleep(Duration::from_millis(CLICK_GAP_MS));
    send_mouse_flag(MOUSEEVENTF_LEFTUP)?;
    Ok(())
}

/// 注入单次鼠标事件（down/up 各调一次，等价 python mouse_event 两次调用）。
/// 审查修复（M11）：SendInput 返回 0 表示注入被拦截/失败（如 UIPI/安全软件），
/// 原实现忽略返回值把失败静默当成功，现透传报错。
fn send_mouse_flag(dw_flags: u32) -> Result<(), String> {
    let input = INPUT {
        r#type: INPUT_MOUSE,
        Anonymous: INPUT_0 {
            mi: MOUSEINPUT {
                dx: 0,
                dy: 0,
                mouseData: 0,
                dwFlags: dw_flags,
                time: 0,
                dwExtraInfo: 0,
            },
        },
    };
    let sent = unsafe { SendInput(1, &input, std::mem::size_of::<INPUT>() as i32) };
    if sent == 0 {
        return Err("鼠标事件注入失败：SendInput 返回 0（可能被安全软件/UIPI 拦截）".to_string());
    }
    Ok(())
}

#[cfg(test)]
mod ui_click_tests {
    use super::*;

    #[test]
    fn click_rejects_out_of_virtual_screen_coords() {
        // 不触达真实鼠标 API：虚拟屏幕边界校验前置失败。
        // 审查修复（M11）：校验语义改为虚拟屏幕边界后，负坐标/零坐标可能是合法
        // （多屏负象限），测试改用任何机器上必然越界的极值坐标
        assert!(click_at(i32::MAX, 100).is_err());
        assert!(click_at(100, i32::MIN).is_err());
        assert!(click_at(i32::MIN, i32::MAX).is_err());
    }
}
