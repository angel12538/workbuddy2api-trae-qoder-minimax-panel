"""#84 未知路径的可诊断性：重复 /v1 前缀给提示，未知 404 记 WARN。"""
import io
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _suite_head import *          # noqa: F401,F403

P, A = bootstrap()
declare_suite(__file__, ["qoder_proxy.py", "docs/clients.md"])

_src = io.open(os.path.join(ROOT, "qoder_proxy.py"), encoding="utf-8").read()

check("#84 有 _unknown_path_error 助手", "def _unknown_path_error(self, path):" in _src)
check("#84 三处兜底 404 都改走它",
      _src.count("return self._unknown_path_error(path)") == 3,
      _src.count("return self._unknown_path_error(path)"))
check("#84 重复前缀给 base URL 提示",
      "base URL 里已经带了 /v1" in _src)
check("#84 未知路径记 WARN（便于自证到达了网关）",
      'level="WARN", tag="http"' in _src.split("def _unknown_path_error")[1][:900])
check("#84 错误体保留路径本身",
      '"not found: %s%s"' in _src)

_doc = io.open(os.path.join(ROOT, "docs", "clients.md"), encoding="utf-8").read()
check("#84 文档补了聚合网关接入章节",
      "接入 new-api" in _doc and "do_request_failed" in _doc)
check("#84 文档给出容器内自检命令",
      "curl -s http://<host>:8790/ping" in _doc)

finish()