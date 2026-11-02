"""#112 流式/非流式 usage 归一为 OpenAI 标准位（cached_tokens）。"""
import io
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py"])

_src = io.open(os.path.join(ROOT, 'qoder_proxy.py'), encoding='utf-8').read()

print("[#112] 三套上游惯例都归一")

# DeepSeek：prompt_cache_hit_tokens
_ds = {'prompt_tokens': 1000, 'completion_tokens': 20, 'total_tokens': 1020,
       'prompt_cache_hit_tokens': 640}
out = P.normalize_usage_for_client(dict(_ds))
check('DeepSeek 的 prompt_cache_hit_tokens → prompt_tokens_details.cached_tokens',
      out['prompt_tokens_details']['cached_tokens'] == 640, out)
check('上游原字段保留（已适配上游形态的客户端不受影响）',
      out.get('prompt_cache_hit_tokens') == 640)

# Qwen：completion_tokens_details.cached_tokens
_qwen = {'prompt_tokens': 500, 'completion_tokens': 10, 'total_tokens': 510,
         'completion_tokens_details': {'cached_tokens': 128,
                                       'reasoning_tokens': 3}}
out = P.normalize_usage_for_client(dict(_qwen))
check('Qwen 的 completion_tokens_details.cached_tokens → 标准位',
      out['prompt_tokens_details']['cached_tokens'] == 128, out)
check('completion_tokens_details 其它字段不动',
      out['completion_tokens_details']['reasoning_tokens'] == 3)

# 已经是标准位：原样
_std = {'prompt_tokens': 10, 'completion_tokens': 1, 'total_tokens': 11,
        'prompt_tokens_details': {'cached_tokens': 7}}
out = P.normalize_usage_for_client(dict(_std))
check('已是标准位时保持不变',
      out['prompt_tokens_details'] == {'cached_tokens': 7}, out)

# 没有任何缓存信息：不凭空造字段
_none = {'prompt_tokens': 10, 'completion_tokens': 1, 'total_tokens': 11}
out = P.normalize_usage_for_client(dict(_none))
check('上游完全没给缓存时不做假（不写空 details）',
      'prompt_tokens_details' not in out, out)
check('0 值也不强塞',
      'prompt_tokens_details' not in P.normalize_usage_for_client(
          {'prompt_tokens': 1, 'prompt_cache_hit_tokens': 0}))
check('非 dict 原样返回', P.normalize_usage_for_client(None) is None)

# 两种都给了：取大的（DeepSeek 字段优先，与 _extract_usage 一致）
_both = {'prompt_tokens': 100, 'prompt_cache_hit_tokens': 40,
         'completion_tokens_details': {'cached_tokens': 90}}
check('两套字段并存时与 _extract_usage 同口径',
      P.normalize_usage_for_client(dict(_both))['prompt_tokens_details']['cached_tokens'] == 40)

print("[#112] 流式出口（clean_chunk）")
_frame = json.dumps({'id': 'chatcmpl-1', 'object': 'chat.completion.chunk',
                     'model': 'DeepSeek-Flash', 'choices': [],
                     'usage': {'prompt_tokens': 900, 'completion_tokens': 5,
                               'total_tokens': 905,
                               'prompt_cache_hit_tokens': 512}})
cleaned = P.clean_chunk(_frame)
_obj = json.loads(cleaned)
check('usage 帧被改写并带上标准位',
      _obj['usage']['prompt_tokens_details']['cached_tokens'] == 512, _obj.get('usage'))
check('改写后仍是合法 JSON 且其它字段不变',
      _obj['id'] == 'chatcmpl-1' and _obj['choices'] == [])

_plain = json.dumps({'id': 'x', 'choices': [{'delta': {'content': 'hi'}}]})
check('普通内容帧逐字节原样透传（不重排、不重编码）',
      P.clean_chunk(_plain) == _plain)
check('没有 usage 的帧不引入 prompt_tokens_details',
      'prompt_tokens_details' not in P.clean_chunk(_plain))

print("[#112] 非流式出口（aggregate_stream）")
check('aggregate_stream 组装 usage 时归一',
      'out["usage"] = normalize_usage_for_client(usage)' in _src)
check('_extract_usage 的三套优先级未变（记账口径不漂移）',
      'usage.get("prompt_cache_hit_tokens")' in _src
      and 'details.get("cached_tokens")' in _src
      and 'prompt_details.get("cached_tokens")' in _src)

finish()