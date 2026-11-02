"""#89 macOS/Linux 常驻：防双开误判 + 官方停止手段。"""
import io
import os
import shutil
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py", "start-qoder-proxy.sh",
                         "stop-qoder-proxy.sh"])

_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()

print("[#89] /health 带身份字段")
check("/health 返回 pid", '"pid": os.getpid(),' in _src)
check("/health 返回 started_at", '"started_at": int(STARTED_AT),' in _src)
check("/health 返回 started_iso", '"started_iso": time.strftime(' in _src)

print("[#89] 实例分类（健康 / 垂死 / 别家 / 无人）")
_alive = lambda pid: pid == 1111
check("无人应答 -> none", P.classify_existing_instance(None) == "none")
check("别的服务 -> foreign",
      P.classify_existing_instance({"service": "wb-proxy", "accounts": 1})
      == "foreign")
check("缺 accounts 字段 -> foreign",
      P.classify_existing_instance({"service": "qoder-proxy"}) == "foreign")
check("pid 活着 -> healthy",
      P.classify_existing_instance({"service": "qoder-proxy", "accounts": 1,
                                    "pid": 1111}, pid_alive=_alive) == "healthy")
check("pid 已退出 -> stale（不再拦启动）",
      P.classify_existing_instance({"service": "qoder-proxy", "accounts": 1,
                                    "pid": 2222}, pid_alive=_alive) == "stale")
check("老版本没有 pid 字段 -> 按 healthy 处理（保守）",
      P.classify_existing_instance({"service": "qoder-proxy", "accounts": 1},
                                   pid_alive=_alive) == "healthy")

print("[#89] 启动流程")
check("stale 时等待端口释放并重试",
      'classify_existing_instance(existing) != "stale"' in _src
      and "旧实例正在释放端口" in _src)
check("bind 失败会重试并在期间复查 /health",
      "bind %s:%d 失败" in _src and "_kind2 == \"healthy\"" in _src)
check("健康双开的提示带上 PID/版本/启动时间",
      'PID: %s · 版本: %s · 启动于: %s' in _src)
check("提示里给出官方停止脚本（跨平台）",
      "stop-qoder-proxy.sh" in _src and "stop-qoder-proxy.bat" in _src)

print("[#89] shell 脚本")
_start = io.open(os.path.join(ROOT, "start-qoder-proxy.sh"), encoding="utf-8").read()
_stop = io.open(os.path.join(ROOT, "stop-qoder-proxy.sh"), encoding="utf-8").read()
check("start.sh 传 --pid-file", "--pid-file" in _start)
check("stop.sh 读 pid 文件并校验命令行",
      "PIDFILE" in _stop and "qoder_proxy.py" in _stop)
check("stop.sh 先 TERM 再 KILL", "kill -TERM" in _stop and "kill -KILL" in _stop)
check("stop.sh 拒绝杀非 qoder-proxy 进程", "refusing to stop" in _stop)
check("stop.sh 无 pid 文件时按端口+进程名兜底",
      '--port $PORT' in _stop)
_bash = shutil.which("bash")
if not _bash:
    print("  [skip] no bash - syntax check skipped")
else:
    for _sh, _name in ((_start, "start"), (_stop, "stop")):
        _f = os.path.join(tempfile.mkdtemp(prefix="shsyn-"), _name + ".sh")
        io.open(_f, "w", encoding="utf-8").write(_sh)
        _r = subprocess.run([_bash, "-n", _f], capture_output=True, text=True)
        check("%s.sh syntax ok" % _name, _r.returncode == 0, (_r.stderr or "")[:160])

print("[#89] 端到端（仅 Linux：Windows 的 kill 与 Windows 进程不互通）")
if not sys.platform.startswith("linux"):
    print("  [skip] 非 Linux 环境跳过真实启停")
else:
    import http.client

    _tmp = tempfile.mkdtemp(prefix="sh89-")
    shutil.copy2(os.path.join(ROOT, "stop-qoder-proxy.sh"), _tmp)
    _pidfile = os.path.join(_tmp, "qoder-proxy.pid")
    _env = dict(os.environ)
    for _k in ("HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "no_proxy"):
        _env.pop(_k, None)
    _env["ACCOUNTS_DIR"] = os.path.join(_tmp, "accounts")
    _env["QD_NO_PAUSE"] = "1"
    _proc = subprocess.Popen(
        [sys.executable, os.path.join(ROOT, "qoder_proxy.py"),
         "--port", "18797", "--pid-file", _pidfile],
        cwd=ROOT, env=_env, stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL)
    _up = False
    for _ in range(40):
        time.sleep(0.5)
        try:
            _c = http.client.HTTPConnection("127.0.0.1", 18797, timeout=2)
            _c.request("GET", "/ping")
            if _c.getresponse().status == 200:
                _up = True
                break
        except Exception:
            pass
    check("服务端成功监听", _up)
    _pid = ""
    if os.path.exists(_pidfile):
        _pid = io.open(_pidfile).read().strip()
    check("服务端写出 pid 文件且内容为自身 PID",
          _pid.isdigit() and int(_pid) == _proc.pid, (_pid, _proc.pid))
    _bash = shutil.which("bash") or "bash"
    _stop_r = subprocess.run(
        [_bash, "-c", "cd %s && bash stop-qoder-proxy.sh 18797" % _tmp],
        capture_output=True, text=True, timeout=60)
    _gone = False
    for _ in range(30):
        if _proc.poll() is not None:
            _gone = True
            break
        time.sleep(0.5)
    check("stop.sh 停掉进程", _gone,
          ((_stop_r.stdout or "")[:150], (_stop_r.stderr or "")[:150]))
    check("stop.sh 清掉 pid 文件", not os.path.exists(_pidfile))
    if _proc.poll() is None:
        _proc.kill()
finish()