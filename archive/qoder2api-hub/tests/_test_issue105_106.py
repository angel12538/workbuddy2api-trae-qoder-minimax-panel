"""#105 上游静默换模型计费可见 + #106 上下文窗口可配置。"""
import io
import json
import os
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py", "qoder_settings.py", "dashboard.html"])
import qoder_settings as ST        # noqa: E402

_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()
_dash = io.open(os.path.join(ROOT, "dashboard.html"), encoding="utf-8").read()

print("[#105] 换模型计费的记录")
_orig_usage = dict(P._usage)
_tmp = tempfile.mkdtemp(prefix="u105-")
_orig_dir_u = P.USAGE_DIR
P.USAGE_DIR = _tmp


def _rows():
    out = []
    with open(os.path.join(P.USAGE_DIR, "usage.jsonl"), encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                out.append(json.loads(line))
    return out


P.record_usage("Kimi-K3", {"prompt_tokens": 10, "completion_tokens": 5,
                             "total_tokens": 15, "model": "auto"})
P.record_usage("Kimi-K3", {"prompt_tokens": 10, "completion_tokens": 5,
                             "total_tokens": 15, "model": "Kimi-K3"})
P.record_usage("DeepSeek-Flash", {"prompt_tokens": 3, "completion_tokens": 1,
                                  "total_tokens": 4})
_r = _rows()
check("不一致时写入 billed_model", _r[0].get("billed_model") == "auto", _r[0])
check("一致时不写 billed_model（行格式零变化）",
      "billed_model" not in _r[1], _r[1])
check("上游没带 model 字段时不写", "billed_model" not in _r[2], _r[2])
check("面板最近请求有计费替换徽标",
      "billed_model" in _dash and _dash.count("badge bad") > 1)
P.USAGE_DIR = _orig_dir_u
P._usage.clear(); P._usage.update(_orig_usage)

print("[#106] 设置存储")
_sd = tempfile.mkdtemp(prefix="st106-")
ST.set_model_context(_sd, "dfmodel", "1M")
check("写入档位", ST.model_context(_sd).get("dfmodel") == "1M",
      ST.model_context(_sd))
ST.set_model_context(_sd, "dfmodel", "1000000")
check("数字串按 int 存", ST.model_context(_sd).get("dfmodel") == 1000000)
ST.set_model_context(_sd, "dfmodel", "")
check("空串=清除", "dfmodel" not in ST.model_context(_sd))

print("[#106] 档位解析与覆盖")
_meta = {"context_config": {"1M": {"token_count": 1000000},
                          "200K": {"token_count": 200000, "is_default": True},
                          "400K": {"token_count": 400000}}}
check("档位名解析", P._context_token_count("1M", _meta) == 1000000)
check("大小写不敏感", P._context_token_count("1m", _meta) == 1000000)
check("纯数字直接返回", P._context_token_count("300000", _meta) == 300000)
check("未列档位返回 None", P._context_token_count("2M", _meta) is None)

_orig_dir = P.ACCOUNTS_DIR
P.ACCOUNTS_DIR = _sd
ST.set_model_context(_sd, "dfmodel", "1M")
ov = P.model_context_override({}, "dfmodel", "DeepSeek-Flash", "cn")
check("settings 覆盖命中", ov == 1000000, ov)
hdr = P.model_context_override({"_qd_context": "400K"}, "dfmodel", "x", "cn")
check("请求头优先于 settings", hdr == 400000, hdr)
check("未设置的模型返回 None",
      P.model_context_override({}, "totally-unknown-model", "x", "cn") is None)
ST.set_model_context(_sd, "dfmodel", "2M")
check("非法档位被忽略（不炸）",
      P.model_context_override({}, "dfmodel", "DeepSeek-Flash", "cn") is None)

print("[#106] build_qoder_body 生效")
ST.set_model_context(_sd, "dfmodel", "1M")
_acc = A.Account({"uid": "u106", "realm": "cn", "accessToken": "t"})
_b1 = P.build_qoder_body({"messages": [{"role": "user", "content": "hi"}]},
                          _acc, "dfmodel", "cn")
check("出站 max_input_tokens 抬到 1M",
      (_b1.get("model_config") or {}).get("max_input_tokens") == 1000000,
      (_b1.get("model_config") or {}).get("max_input_tokens"))
_cc = (_b1.get("chat_context") or {}).get("extra") or {}
_mc2 = _cc.get("modelConfig") or {}
check("chat_context 副本同步",
      _mc2.get("max_input_tokens") == 1000000, _mc2.get("max_input_tokens"))
_b2 = P.build_qoder_body({"messages": [{"role": "user", "content": "hi"}],
                          "_qd_context": "400K"}, _acc, "dfmodel", "cn")
check("请求头档位生效",
      (_b2.get("model_config") or {}).get("max_input_tokens") == 400000)
P.ACCOUNTS_DIR = _orig_dir

print("[#106] 防伪造与路由")
check("入口先 pop 客户端伪造键再读 header",
      'target.pop("_qd_context", None)' in _src
      and "X-QD-Context" in _src)
check("三个入口都注入",
      _src.count("self._apply_context_header(") == 3,
      _src.count("self._apply_context_header("))
check("/settings 输出 model_context",
      '"model_context": _model_context_snapshot()' in _src)
_route = _src[_src.index("/settings/model-context"):]
check("POST /settings/model-context 需面板鉴权",
      "panel password required" in _route[:400])
check("面板模型库有上下文下拉",
      "saveModelContext" in _dash and "ctx-select" in _dash)

finish()