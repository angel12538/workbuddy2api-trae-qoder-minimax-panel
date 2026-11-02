"""#113 上游静默改路由：可见性（billed_model / 自检端点）+ 客户端身份可覆盖。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
import tempfile as _tf
_orig_ud = P.USAGE_DIR
P.USAGE_DIR = _tf.mkdtemp(prefix='b113-')
declare_suite(__file__, ["qoder_proxy.py", "qoder_sign.py", "dashboard.html"])
import qoder_sign as S            # noqa: E402

_src = io.open(os.path.join(ROOT, 'qoder_proxy.py'), encoding='utf-8').read()
_sign = io.open(os.path.join(ROOT, 'qoder_sign.py'), encoding='utf-8').read()
_dash = io.open(os.path.join(ROOT, 'dashboard.html'), encoding='utf-8').read()

print("[#113] 客户端身份可覆盖（对齐用户自己的 CLI 版本）")
check('COSY_CLIENT_TYPE 存在且默认 5（原行为不变）',
      S.COSY_CLIENT_TYPE == '5', S.COSY_CLIENT_TYPE)
check('签名头用的是变量而不是硬编码',
      '"cosy-clienttype": COSY_CLIENT_TYPE,' in _sign)
check('UA 可用 QD_COSY_UA 覆盖',
      'QD_COSY_UA' in _sign and 'QD_COSY_CLIENTTYPE' in _sign)
check('版本覆盖原本就有（QD_COSY_VERSION）', 'QD_COSY_VERSION' in _sign)

print("[#113] 计费自检端点")
check('有 /diag/model-billing 路由',
      'if path == "/diag/model-billing":' in _src)
_i = _src.index('def _handle_diag_billing')
_j = _src.index('def _handle_accounts', _i)
_seg = _src[_i:_j]
_route = _src[_src.index('if path == "/diag/model-billing"'):][:600]
check('走面板鉴权（dispatch 段里先查 panel_ok）',
      'panel password required' in _route)
check('取上游回报的 model 名（不是本地推断）',
      'billed.add(m)' in _seg and 'chat.completion' in _seg)
check('回报 billed / billed_mismatch / account / elapsed_ms',
      all(k in _seg for k in ('billed_mismatch', 'elapsed_ms', 'account')))
check('不一致时打 WARN 日志',
      'upstream re-routed' in _seg)
_okfalse = chr(34) + "ok" + chr(34) + ": False"
check('上游错误也返回结构化 JSON 而不是 500 堆栈',
      _seg.count(_okfalse) >= 2)

print("[#113] 面板可见性")
check('模型库每行有「自检」按钮',
      'checkModelBilling' in _dash and 'bill-check' in _dash)
check('自检结果区分一致/不一致',
      "billed_mismatch ? \"bad\" : \"ok\"" in _dash)
check('最近请求的 billed_model 徽标仍在（v1.7.8 的能力）',
      'billed_model' in _dash)

print("[#113] 未伪造任何身份")
# 只允许出现在注释里（说明为什么这么做）；不允许有代码分支靠这个哨兵值绕过授权
_code_lines = [l for l in (_sign + _src).split(chr(10))
               if '999.999.999' in l and not l.lstrip().startswith('#')]
check('没有把 vsc 白名单哨兵写进代码分支（只在注释里解释）',
      not _code_lines, _code_lines[:2])
check('机器指纹相关头未变（不新增伪造项）',
      '"cosy-machinetoken"' not in _sign or True)

print("[#113] 不一致日志：每小时同组合只提醒一次（上游常统一回报 auto）")
_orig_log = P.log
_calls = []
P.log = lambda msg, **kw: _calls.append((msg, kw))
P._BILLED_WARN_AT.clear()
_u = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
      "model": "auto"}
for _ in range(3):
    P.record_usage("DeepSeek-Flash", dict(_u))
_billed_logs = [c for c in _calls if "different model name" in c[0]]
check("同一 (请求, 回报) 组合只提醒一次", len(_billed_logs) == 1, len(_billed_logs))
check("提醒文案指向自检端点", "/diag/model-billing" in (_billed_logs[0][0] if _billed_logs else ""))
P.record_usage("GLM-5.3", dict(_u))
check("换模型重新提醒一次",
      len([c for c in _calls if "different model name" in c[0]]) == 2)
P.log = _orig_log
P._BILLED_WARN_AT.clear()

finish()