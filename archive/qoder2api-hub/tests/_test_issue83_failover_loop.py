"""#83 跨区 failover 死循环：realm=None 时不得把「另一区」解析成同一区，且只切一次。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

_real_ready = P._realm_ready_total
_real_detect = P.detect_model_realm
_real_current = P.CURRENT_REALM


def _stub_ready(fn, detect="cn", current="cn"):
    P._realm_ready_total = fn
    P.detect_model_realm = lambda m: detect
    P.CURRENT_REALM = current


try:
    # ---------------------------------------------------------- 解析后的另一区
    _stub_ready(lambda realm, model=None: 2)
    check("#83 realm=cn -> intl", P._alt_realm_ready("cn", "DeepSeek-Flash") == "intl")
    check("#83 realm=intl -> cn", P._alt_realm_ready("intl", "DeepSeek-Flash") == "cn")
    _none_res = P._alt_realm_ready(None, "DeepSeek-Flash")
    check("#83 realm=None 解析为 cn 后，另一区必须是 intl（不能原地打转）",
          _none_res == "intl", _none_res)
    check("#83 realm=None 的结果不等于解析出的本区",
          _none_res not in ("", "cn") or _none_res == "", _none_res)

    # intl 独占模型：解析出 intl，另一区应是 cn
    _stub_ready(lambda realm, model=None: 2, detect="intl")
    check("#83 intl 独占模型 + realm=None -> cn",
          P._alt_realm_ready(None, "Ultimate") == "cn",
          P._alt_realm_ready(None, "Ultimate"))

    # 另一区没有就绪账号 -> 返回空（不切）
    _stub_ready(lambda realm, model=None: 0 if realm == "intl" else 3)
    check("#83 另一区无就绪账号时不跨区",
          P._alt_realm_ready(None, "DeepSeek-Flash") == "")
    # 探测异常 -> 返回空，不阻断主流程
    def _boom(realm, model=None):
        raise RuntimeError("boom")
    _stub_ready(_boom)
    check("#83 探测异常时安全返回空", P._alt_realm_ready("cn", "x") == "")
finally:
    P._realm_ready_total = _real_ready
    P.detect_model_realm = _real_detect
    P.CURRENT_REALM = _real_current

# ---------------------------------------------------------- 源码守卫：只切一次
_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
check("#83 两个流式循环都有「只切一次」标记", _src.count("_switched71 = False") == 2,
      _src.count("_switched71 = False"))
check("#83 failover 条件包含 not _switched71",
      _src.count("and not _switched71):") == 2,
      _src.count("and not _switched71):"))
check("#83 切换后立刻置位（避免重复切同一区）",
      _src.count("_switched71 = True") == 2, _src.count("_switched71 = True"))
check("#83 跨区目标用解析后的「实际出口」",
      "base = realm or detect_model_realm(model) or CURRENT_REALM" in _src)

finish()