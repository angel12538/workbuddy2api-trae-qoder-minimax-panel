"""#85 启动 bat 必须能处理带空格的路径。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["start-qoder-proxy.bat", "stop-qoder-proxy.bat"])

_start = io.open(os.path.join(ROOT, "start-qoder-proxy.bat"), encoding="utf-8").read()

print("[#85] 参数引号")
check("不再直接裸拼 -ArgumentList '%SCRIPT%'",
      "-ArgumentList '%SCRIPT%'" not in _start)
check("用 [char]34 生成引号（避开双重转义）",
      "[char]34" in _start)
check("路径经环境变量传入（避开 cmd 元字符）",
      "$env:QD_SCRIPT" in _start and "$env:QD_PIDFILE" in _start)
check("脚本路径与 pid 路径都被加引号",
      _start.count("$q + $env:QD_SCRIPT + $q") == 1
      and _start.count("$q + $env:QD_PIDFILE + $q") == 1)
check("日志重定向用变量（空格安全）",
      "-RedirectStandardOutput $env:QD_LOGFILE" in _start)
check("启动方式仍是隐藏窗口 + PassThru（#50 语义不变）",
      "-WindowStyle Hidden" in _start and "-PassThru" in _start)
check("bat 仍保持 ASCII（无中文注释）",
      all(ord(ch) < 128 for ch in _start))

print("[#85] 端到端（临时目录带空格 + 桩脚本）")
if os.name != 'nt':
    print("  [skip] Windows-only 端到端用例")
else:

    import json
    import shutil
    import subprocess
    import tempfile
    import time

    _tmp = tempfile.mkdtemp(prefix="qd space-")
    _dst = os.path.join(_tmp, "my project", "qoder proxy")
    os.makedirs(_dst, exist_ok=True)
    shutil.copy2(os.path.join(ROOT, "start-qoder-proxy.bat"), _dst)
    _argv_path = os.path.join(_dst, "argv.json")
    io.open(os.path.join(_dst, "qoder_proxy.py"), "w", encoding="utf-8").write(
        "import json, sys\n"
        "open(r'%s', 'w', encoding='utf-8').write(json.dumps(sys.argv, ensure_ascii=False))\n"
        % _argv_path.replace("\\", "\\\\"))
    _env = dict(os.environ, QD_NO_PAUSE="1")
    for _k in ("HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "no_proxy"):
        _env.pop(_k, None)
    _run = subprocess.run(["cmd", "/c", "start-qoder-proxy.bat", "18766"], cwd=_dst,
                          env=_env, capture_output=True, text=True, timeout=90,
                          errors="replace")
    for _ in range(20):
        if os.path.exists(_argv_path):
            break
        time.sleep(0.5)
    _argv = []
    if os.path.exists(_argv_path):
        try:
            _argv = json.loads(io.open(_argv_path, encoding="utf-8").read())
        except Exception:
            _argv = []
    check("带空格目录下脚本被当作**一个**参数传入",
          bool(_argv) and _argv[0].endswith("qoder_proxy.py")
          and " " in _argv[0], _argv[:2])
    check("--port / --pid-file 正常拆分",
          "--port" in _argv and "18766" in _argv
          and "--pid-file" in _argv and _argv[-1].endswith("qoder-proxy.pid"),
          _argv)
finish()