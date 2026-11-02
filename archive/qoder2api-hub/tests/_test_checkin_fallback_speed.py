"""国际版签到基址回退 + 签到并发提速。"""
import io
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_accounts.py", "qoder_tasks.py", "qoder_proxy.py"])
import qoder_tasks as T            # noqa: E402

print("[checkin] openapi 基址候选")
_cands_intl = A.openapi_candidates("intl")
check("intl 首选配置里的 openapi",
      _cands_intl[0] == A.REALM_CONFIGS["intl"]["openapi"].rstrip("/"), _cands_intl)
check("intl 至少有两个候选（可回退）", len(_cands_intl) >= 2, _cands_intl)
check("候选去重", len(_cands_intl) == len(set(_cands_intl)))
_old_env = os.environ.get("QD_INTL_OPENAPI")
os.environ["QD_INTL_OPENAPI"] = "https://openapi.example.test"
try:
    _cands_env = A.openapi_candidates("intl")
finally:
    if _old_env is None:
        os.environ.pop("QD_INTL_OPENAPI", None)
    else:
        os.environ["QD_INTL_OPENAPI"] = _old_env
check("QD_<REALM>_OPENAPI 可覆盖（排在最前）",
      _cands_env[0] == "https://openapi.example.test", _cands_env)
check("cn 候选同样可用", len(A.openapi_candidates("cn")) >= 1)

_src_acc = io.open(os.path.join(ROOT, "qoder_accounts.py"), encoding="utf-8").read()
check("有 _openapi_json 助手", "def _openapi_json(" in _src_acc)
check("活动列表走候选基址",
      "self._openapi_json(PATH_CAMPAIGNS" in _src_acc)
check("签到状态走候选基址",
      "self._openapi_json(PATH_CHECKIN_STATUS" in _src_acc)
check("签到领取走候选基址",
      "self._openapi_json(PATH_CHECKIN_CLAIM" in _src_acc)
check("仅在 404/连接失败时换基址（业务错误不换）",
      "if exc.code in (404, 405, 410):" in _src_acc
      and "return last" in _src_acc)

print("[checkin] 并发提速")

_src_tasks = io.open(os.path.join(ROOT, "qoder_tasks.py"), encoding="utf-8").read()
check("批量签到并发执行", "ThreadPoolExecutor(max_workers=workers)" in _src_tasks)
check("非首个任务抖动错开（0~0.3s，首个立即开始）",
      "random.uniform(0, 0.3)" in _src_tasks and "if idx:" in _src_tasks)
check("并发度可配 QD_CHECKIN_CONCURRENCY",
      "QD_CHECKIN_CONCURRENCY" in _src_tasks)
check("日志按账号顺序输出（blocks 索引回填）",
      "blocks[idx] = lines" in _src_tasks)

# 行为：3 个账号并发应显著快于串行（每个 stub 睡 0.35s）
class _Acc:
    def __init__(self, i):
        self.uid = "u%d" % i
        self.nickname = "n%d" % i
        self.enabled = True
        self.access_token = "t"

_real_run = T.run_checkin
def _fake(acc, gap=1.0, only_daily=False, claim_gap=None):
    time.sleep(0.35)
    return {"ok": True, "earned_credit": 100, "logs": ["ok " + acc.uid]}
T.run_checkin = _fake
try:
    t0 = time.time()
    res = T.run_batch_checkin([_Acc(0), _Acc(1), _Acc(2)], gap=0.0)
    elapsed = time.time() - t0
finally:
    T.run_checkin = _real_run
check("3 个账号并发耗时明显小于串行（<0.8s，串行需 ~1.05s+）",
      elapsed < 0.8, round(elapsed, 2))
check("结果统计正确（3 成功 / +300）",
      res["accounts_count"] == 3 and res["credit_added"] == 300, res.get("credit_added"))
_order = [ln for ln in res["logs"] if ln.startswith("======") and "正在为账号" in ln]
check("日志按账号顺序输出",
      [ln.split("[")[1].split(" ")[0] for ln in _order] == ["n0", "n1", "n2"],
      [ln[:40] for ln in _order])

_src_proxy = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("面板签到路由也并发",
      "qd:checkin-concurrency" not in _src_proxy
      and "ThreadPoolExecutor(max_workers=workers)" in _src_proxy)

finish()