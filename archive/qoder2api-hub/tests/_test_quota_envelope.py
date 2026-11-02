# -*- coding: utf-8 -*-
"""#108 额度类 403（112 额度耗尽 / 110 每日次数上限）快速失败套件。

    python tests/_test_quota_envelope.py

钉住五件事：
  1) 112 -> 账号级冷却 1800s，**单号池也不被** single_account 压成 3s；
  2) 110 -> 冷却到下一个本地 0 点（封顶 6h，不短于 60s）；
  3) 112/110 不禁用账号、不喂降权（配额问题不是凭证失效/未知故障）；
  4) 10605 / isQueued 仍然零冷却（#71 的既有行为不能被这次改动破坏）；
  5) 两条分类点（envelope + open_upstream）都接了额度判断。
"""
import os
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py", "qoder_accounts.py"])

MODEL = "gfmodel"

# 上游原文（2026-10-27 现场日志；112 的 message 是二次转义的 JSON 字符串）
D112 = ('{"code":"112","message":"{\\"pricingUrl\\":\\"https://qoder.com.cn/'
        'pricing?client=qoder\\"}"}')
D110 = '{"code":"110","message":"Billing daily count exceeded"}'
D10605 = ('{"code":"403","message":"{\\"code\\":\\"10605\\",\\"isQueued\\":true,'
          '\\"retryAfterSeconds\\":30}"}')


def build(n=1, realm="cn"):
    """真实 Account + AccountPool：下面的冷却数字由**真的** note_error 算出来。"""
    accs = []
    for i in range(n):
        a = A.Account({"uid": "q108-%d" % i, "realm": realm,
                       "accessToken": "dt-q108-%d" % i})
        a.expires_at = time.time() + 86400
        accs.append(a)
    pool = A.AccountPool(tempfile.mkdtemp(), log=lambda *a, **k: None)
    pool.accounts = accs
    return pool, accs


def wait_of(acc):
    """本账号还要等多少秒才能再服务 MODEL（0 = 现在就能用）。"""
    return acc.throttle_wait(model=MODEL)


# ---------------------------------------------------------------- 判据
check("#108 112 认得（现场原文，含 pricingUrl）",
      P._quota_cooldown(D112) == ("exhausted", 1800), P._quota_cooldown(D112))
check("#108 112 认得（日志里的转义形态 \\\"code\\\":\\\"112\\\"）",
      P._quota_cooldown('{\\"code\\":\\"112\\"}')[0] == "exhausted",
      P._quota_cooldown('{\\"code\\":\\"112\\"}'))
check("#108 110 认得（原文 Billing daily count exceeded）",
      P._quota_cooldown(D110)[0] == "daily", P._quota_cooldown(D110))
check("#108 110 认得（纯 code 形态）",
      P._quota_cooldown('{"code":"110"}')[0] == "daily")
check("#108 普通 403 不误判成额度",
      P._quota_cooldown('{"code":"forbidden"}') == ("", 0),
      P._quota_cooldown('{"code":"forbidden"}'))
check("#108 10605 排队不误判成额度",
      P._quota_cooldown(D10605)[0] == "", P._quota_cooldown(D10605))

# ---------------------------------------------------------------- 112 单号池
P.POOL, _accs = build(1)
_ONE = _accs[0]
check("#108 单号池口径 = 1（下面「不被 clamp」的断言才有证据）",
      P._realm_ready_total("cn", MODEL) == 1, P._realm_ready_total("cn", MODEL))
P._handle_envelope_account_cooldown(_ONE, P.UpstreamStatus(403, D112), model=MODEL)
_w = wait_of(_ONE)
check("#108 112 -> 冷却 1800s（30 分钟）", 1790 <= _w <= 1810, _w)
check("#108 112 单号池不被 single_account 压成 3s", _w > 60, _w)
check("#108 112 不禁用账号（配额不是凭证失效）", _ONE.enabled is True, _ONE.enabled)
check("#108 112 不喂降权（配额不是未知故障）", _ONE.degrade_count == 0, _ONE.degrade_count)
check("#108 112 不写模型级冷却（配额是账号级的）",
      not _ONE.model_cooldowns, _ONE.model_cooldowns)

# ---------------------------------------------------------------- 110 单号池
P.POOL, _accs = build(1)
_TWO = _accs[0]
P._handle_envelope_account_cooldown(_TWO, P.UpstreamStatus(403, D110), model=MODEL)
_w2 = wait_of(_TWO)
_now = time.time()
_mid = time.mktime(time.localtime(_now)[:3] + (0, 0, 0, 0, 0, -1)) + 86400
_expect = min(6 * 3600, _mid - _now)
check("#108 110 -> 冷却到本地 0 点（封顶 6h）", abs(_w2 - _expect) <= 5, (_w2, _expect))
check("#108 110 冷却不短于 60s", _w2 >= 59, _w2)
check("#108 110 不禁用账号", _TWO.enabled is True, _TWO.enabled)

# ---------------------------------------------------------------- 10605 不回归
P.POOL, _accs = build(1)
_Q = _accs[0]
P._handle_envelope_account_cooldown(_Q, P.UpstreamStatus(403, D10605), model=MODEL)
check("#108 10605 排队仍然零冷却（#71 不被破坏）",
      wait_of(_Q) <= 0.001 and not _Q.model_cooldowns,
      (wait_of(_Q), _Q.model_cooldowns))

# ---------------------------------------------------------------- 多号池也照冷
P.POOL, _accs = build(2)
_M = _accs[0]
P._handle_envelope_account_cooldown(_M, P.UpstreamStatus(403, D112), model=MODEL)
check("#108 多号池下 112 同样冷却 1800s", 1790 <= wait_of(_M) <= 1810, wait_of(_M))

# ---------------------------------------------------------------- 客户端文案
_msg, _kind = P.friendly_upstream_error(403, D112)
check("#108 客户端文案点明额度耗尽", "额度耗尽" in _msg and _kind == "quota_exhausted",
      (_kind, _msg[:60]))
check("#108 排队文案不被额度文案抢走",
      P.friendly_upstream_error(403, D10605)[1] == "upstream_queued",
      P.friendly_upstream_error(403, D10605)[1])

# ---------------------------------------------------------------- 接线
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#108 两条分类点都接了额度判断（envelope + open_upstream）",
      _src.count("if _note_quota_cooldown(") == 2,
      _src.count("if _note_quota_cooldown("))
check("#108 额度判断排在 10605 短路段之后（排队不被额度逻辑吃掉）",
      _src.index("_is_queue71") < _src.index("if _note_quota_cooldown("))

finish()
