"""一键更新：更新器 + 端点守卫 + 面板接线。

    python tests/_test_update_oneclick.py
"""
import io
import os
import sys
import tarfile
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_updater.py", "qoder_proxy.py", "dashboard.html"])
import qoder_updater as U          # noqa: E402


def _write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with io.open(path, "w", encoding="utf-8") as fh:
        fh.write(text)


# ---------------------------------------------------------------- 文件选择
_tmp = tempfile.mkdtemp()
_rel = os.path.join(_tmp, "repo-1.0.0")
_write(os.path.join(_rel, "qoder_proxy.py"), "VERSION = \"1.0.0\"\n")
_write(os.path.join(_rel, "dashboard.html"), "<html></html>")
_write(os.path.join(_rel, "docs", "a.md"), "# a")
_write(os.path.join(_rel, "accounts", "leak.json"), "{}")
_write(os.path.join(_rel, "python", "runtime.dll"), "x")
_write(os.path.join(_rel, "backups", "old.py"), "x")
_write(os.path.join(_rel, "notes.exe"), "x")
_files = U.plan_update(_rel)
check("计划只包含可更新文件（py/html/md）",
      "qoder_proxy.py" in _files and "dashboard.html" in _files
      and "docs/a.md" in _files, _files)
check("计划跳过 accounts/ python/ backups/ 等保护路径",
      not any(f.startswith(("accounts/", "python/", "backups/"))
              for f in _files), _files)
check("计划跳过非代码后缀（.exe）",
      not any(f.endswith(".exe") for f in _files), _files)

# ---------------------------------------------------------------- 校验
check("release VERSION 与 tag 一致时校验通过",
      U._verify_release(_rel, "v1.0.0") == "",
      U._verify_release(_rel, "v1.0.0"))
check("release VERSION 与 tag 不一致时拒绝",
      "version mismatch" in U._verify_release(_rel, "v2.0.0"),
      U._verify_release(_rel, "v2.0.0"))
_write(os.path.join(_rel, "broken.py"), "def x(:\n")
check("release 里有语法错误的 py 时拒绝",
      "broken python" in U._verify_release(_rel, "v1.0.0"),
      U._verify_release(_rel, "v1.0.0")[:80])

# ---------------------------------------------------------------- 解压安全
_src_dir = os.path.join(_tmp, "src")
os.makedirs(_src_dir, exist_ok=True)
_tar = os.path.join(_tmp, "evil.tar.gz")
with tarfile.open(_tar, "w:gz") as tf:
    # 恶意成员：解压后应落在 _tmp/evil.txt（穿越出 dest）
    _bad = os.path.join(_src_dir, "evil.txt")
    with io.open(_bad, "w", encoding="utf-8") as fh:
        fh.write("x")
    tf.add(_bad, arcname="repo-1.0.0/../../evil.txt")
    _ok = os.path.join(_src_dir, "ok.txt")
    with io.open(_ok, "w", encoding="utf-8") as fh:
        fh.write("x")
    tf.add(_ok, arcname="repo-1.0.0/ok.txt")
_dest = os.path.join(_tmp, "x")
_root = U._extract(_tar, _dest)
check("解压过滤路径穿越（.. / 绝对路径）",
      not os.path.exists(os.path.join(_tmp, "evil.txt"))
      and os.path.isfile(os.path.join(_root, "ok.txt")))

# ---------------------------------------------------------------- apply 语义
_real_latest = U.latest_release
U.latest_release = lambda timeout=8: {"tag": "v1.0.0", "version": "1.0.0",
                                       "url": "", "tarball": ""}
try:
    _noop = U.apply_update(_rel)
finally:
    U.latest_release = _real_latest
check("已是最新时 apply 直接返回 noop（不下载）",
      _noop.get("ok") is True and _noop.get("noop") is True, _noop.get("message"))
_old_container = U._in_container
U._in_container = lambda: True
try:
    _cont = U.apply_update(_rel)
finally:
    U._in_container = _old_container
check("容器里拒绝自更新并给出 docker 指令",
      _cont.get("ok") is False and "docker compose" in (_cont.get("error") or ""),
      _cont.get("error"))

# ---------------------------------------------------------------- 端点守卫
check("loopback 判定",
      P._is_loopback("127.0.0.1") and P._is_loopback("::1")
      and not P._is_loopback("10.0.0.5"))
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("POST /update/apply 存在", 'if path == "/update/apply":' in _src)
check("更新端点要求面板会话",
      'if path == "/update/apply":' in _src
      and _src.split('if path == "/update/apply":')[1][:400].count("_panel_ok()") >= 1)
check("更新端点默认只允许本机",
      "_update_allow_remote()" in _src and "_is_loopback(" in _src)
check("支持 QD_UPDATE_DISABLE 禁用", "QD_UPDATE_DISABLE" in _src)
check("更新后自动重启（按原 argv）",
      "schedule_restart(_LAUNCH_ARGV" in _src and "_LAUNCH_ARGV = list(sys.argv)" in _src)
check("GET /update/plan 预检存在", 'if path == "/update/plan":' in _src)

# ---------------------------------------------------------------- 面板接线
_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()
check("面板有「一键更新」按钮", "applyUpdate(this)" in _dash and "一键更新" in _dash)
check("面板调用 /update/apply", "'/update/apply'" in _dash)
check("更新后轮询 /health 并自动刷新",
      "waitForRestart" in _dash and "location.reload()" in _dash)
check("更新前有确认提示", "confirm(" in _dash.split("function applyUpdate")[1][:400])
check("schedule_restart 无 argv 时返回 False", U.schedule_restart([]) is False)

finish()