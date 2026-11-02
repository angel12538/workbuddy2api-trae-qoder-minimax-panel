"""跑 tests/ 下的所有套件，每个套件打印一行汇总（P0-2 测试工程化 · 骨架）。

    python tests/run_all.py            # 全部
    python tests/run_all.py guards     # 只跑名字里含 guards 的套件

编排照抄 wb 的 tests/run_all.py：并发执行、每套件独立日志文件、失败打印 tail 25 行、
强制 UTF-8（子进程 stdout 编码 + 日志写入）。套件名规范 _test_*.py（.js 需要 node）。
"""
import concurrent.futures as _cf
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
TAIL_LINES = 25
JOBS = max(1, int(os.environ.get("QD_TESTS_JOBS") or 4))


def suites(pattern=""):
    names = sorted(n for n in os.listdir(HERE)
                   if n.startswith("_test_") and n.endswith((".py", ".js")))
    return [n for n in names if pattern in n]


def command(name):
    path = os.path.join(HERE, name)
    if name.endswith(".js"):
        return ["node", path]
    return [sys.executable, path]


def tail(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            lines = [line.rstrip() for line in fh if line.strip()]
    except OSError:
        return []
    return lines[-TAIL_LINES:]


def total_of(path):
    """从套件日志里取 `SUMMARY: TOTAL N checks` 的 N（迁移期间用它保证总数不下降）。"""
    import re
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            m = None
            for line in fh:
                hit = re.search(r"SUMMARY: TOTAL (\d+) checks", line)
                if hit:
                    m = int(hit.group(1))
            return m
    except OSError:
        return None


def _ensure_suite_line(name, log):
    """runner 兜底（拿铁建议）：套件没自己打 SUITE: 行时，由 runner 补一条**显式标注**的，
    这样「跑的是哪份套件」永远有证据，且不需要动任何人的文件。

    注意只兜「套件身份」（SUITE md5）—— 自动可得、不含领域知识；
    「守护哪些被测文件」（FILE: 行）需要领域知识，代填有填错的风险（声明错比不声明更糟），
    留给各 owner 在下一次触碰该套件时补。
    """
    try:
        with open(log, encoding="utf-8", errors="replace") as fh:
            body = fh.read()
    except OSError as exc:
        # 保险自己的失败也要留痕（拿铁指出的真模式）：原来这里是 return None，
        # 于是「零触发」与「坏了」在输出上完全一样 —— 正是我们反复在修的
        # 「不把『不知道』渲染成『没发生』」。
        return ("SUITE: %s md5=<unavailable> "
                "(runner fallback FAILED: cannot read log: %s)" % (name, exc))
    # 判据收紧（拿铁指出的边界）：只含 "SUITE:" 不够 —— 套件若 import 了别的套件
    # （如 _test_legacy.py import 主套件），日志里会有**别人**的 SUITE 行，兜底就漏了。
    if ("SUITE: %s" % name) in body:
        return None
    import hashlib
    path = os.path.join(HERE, name)
    try:
        raw = open(path, "rb").read()
    except OSError as exc:
        return "SUITE: %s md5=<unavailable> (runner fallback FAILED: cannot read suite: %s)" % (name, exc)
    line = "SUITE: %s md5=%s (runner-computed; suite did not declare)" % (
        name, hashlib.md5(raw).hexdigest())
    # 同时追加进该套件的 log（拿铁的实测建议：logs: <dir> 这个提示让人以为「日志才是留存
    # 证据」，只保存日志的人复查时会以为兜底没生效）。
    try:
        with open(log, "a", encoding="utf-8") as fh:
            fh.write("\n" + line + "\n")
    except OSError as exc:
        return line + "  [log append FAILED: %s]" % exc
    return line


def run_one(name, logdir, env):
    log = os.path.join(logdir, name + ".log")
    with open(log, "wb") as fh:
        rc = subprocess.call(command(name), stdout=fh,
                             stderr=subprocess.STDOUT, env=env, cwd=HERE)
    return name, rc, log


def main(argv):
    if len(argv) > 1 and argv[1] in ("-h", "--help"):
        print(__doc__)
        return 0
    # 兼容两种调用：python tests/run_all.py dashboard（位置参数=过滤模式）
    # 与 --jobs 4 --logs suite-logs（CI：日志落可见目录、限制并发）。
    # wb 同款接口——CI 步骤与本地命令一条心。
    import argparse
    ap = argparse.ArgumentParser(add_help=False)
    ap.add_argument("pattern", nargs="?", default="")
    ap.add_argument("--jobs", type=int, default=0)
    ap.add_argument("--logs", default="")
    known, _rest = ap.parse_known_args(argv[1:])
    pattern = known.pattern
    jobs = known.jobs or JOBS
    logdir = known.logs or tempfile.mkdtemp(prefix="qd-suites-")
    if known.logs:
        os.makedirs(known.logs, exist_ok=True)
    selected = suites(pattern)
    if not selected:
        print("  no suite matches %r in %s" % (pattern, HERE))
        return 2
    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join(
        [ROOT] + ([env["PYTHONPATH"]] if env.get("PYTHONPATH") else []))
    env["PYTHONIOENCODING"] = "utf-8"
    if not shutil.which("node"):
        env.pop("NODE_OK", None)
    # 测试用量目录：所有套件都走临时目录，一个字节都不写仓库的 usage/。
    # 这里是**唯一能可靠覆盖所有套件**的地方 —— 有些套件（_test_model_gates /
    # _test_anthropic_wiring / _test_failure_families / _test_credits_guard）
    # 绕过 bootstrap() 自己 import qoder_proxy，靠 _suite_head 的 setdefault
    # 管不到它们，必须由 runner 直接注入环境变量。
    _acc_usage = os.path.join(ROOT, "_acc", "usage")
    env.setdefault("QD_PROXY_USAGE_DIR", _acc_usage)
    # issue #87：测试里的本机 HTTP 不能被系统代理劫持。开发机常设
    # HTTP(S)_PROXY（如 127.0.0.1:7897），一旦带上，套件对 127.0.0.1 的请求会
    # 走代理并被回 502 —— 表现为「整批套件莫名全红」，而真实代码没问题。
    # 这里在**子进程环境**里统一清掉（个别套件需要时会自己 set）。
    for _k in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
               "http_proxy", "https_proxy", "all_proxy",
               "NO_PROXY", "no_proxy"):
        env.pop(_k, None)
    # 跑前清一次：套件每次请求都会 append 一行，不清则越跑越大。
    # 只删隔离区，真实用量在 <ROOT>/usage，绝不被碰。
    # 不用 ignore_errors：Windows 上杀软/残留句柄导致的 PermissionError 会被
    # 吞掉，目录没清却看着「清过了」。清不掉就直说。
    if os.path.isdir(_acc_usage):
        try:
            shutil.rmtree(_acc_usage)
        except OSError as exc:
            print("  [warn] could not clear %s: %s" % (_acc_usage, exc))
    with _cf.ThreadPoolExecutor(max_workers=jobs) as ex:
        results = list(ex.map(lambda n: run_one(n, logdir, env), selected))
    failed = []
    grand = 0
    unknown = []
    for name, rc, log in results:
        n = total_of(log)
        if n is None:
            unknown.append(name)
        else:
            grand += n
        print("  %-34s %-10s checks=%s" % (name, "PASS" if rc == 0 else "FAIL(rc=%d)" % rc,
                                            n if n is not None else "?"))
        _fb = _ensure_suite_line(name, log)
        if _fb:
            print("      " + _fb)
        if rc != 0:
            failed.append((name, log))
    for name, log in failed:
        print("")
        print("---- %s tail (%d lines) ----" % (name, TAIL_LINES))
        for line in tail(log):
            print("    " + line)
    print("")
    print("SUMMARY: %d suites, %d passed, %d failed, TOTAL %d checks%s  (logs: %s)"
          % (len(results), len(results) - len(failed), len(failed), grand,
             ("" if not unknown else " (+%d suites without SUMMARY: %s)"
              % (len(unknown), ", ".join(unknown))), logdir))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
