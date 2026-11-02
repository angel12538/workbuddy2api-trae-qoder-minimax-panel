"""Exercise the model-list console renderer with a small Node DOM shim."""

from __future__ import annotations

import json
import re
import shutil
import subprocess
from pathlib import Path

import pytest

from src import main


def _refresh_models_source() -> str:
    html = main._web_login_html()
    match = re.search(
        r"(async function refreshModels\(\)\{.*?\n\})\nasync function logout",
        html,
        re.DOTALL,
    )
    assert match, "could not extract refreshModels from the rendered console"
    return match.group(1)


def _run_console_renderer(models: list[dict] | None = None, *, fail: bool = False) -> None:
    node = shutil.which("node")
    if not node:
        pytest.skip("Node is not installed for console JavaScript verification")

    source = _refresh_models_source()
    fixture = json.dumps(models or [], ensure_ascii=False)
    javascript = r"""
const vm = require('node:vm');
const assert = require('node:assert/strict');

const SOURCE = __SOURCE__;
const MODELS = __MODELS__;
const SHOULD_FAIL = __SHOULD_FAIL__;

class Element {
  constructor(id = '') {
    this.id = id;
    this.className = '';
    this.textContent = '';
    this.hidden = false;
    this.disabled = false;
    this.children = [];
    this._innerHTML = '';
  }
  set innerHTML(value) {
    this._innerHTML = String(value);
    this.children = [];
  }
  get innerHTML() { return this._innerHTML; }
  appendChild(child) {
    this.children.push(child);
    return child;
  }
}

async function run() {
  const elements = new Map();
  const ids = [
    'models-refresh-btn', 'model-list-meta', 'models-col-remote',
    'models-col-traecode', 'models-col-traework', 'models-count-remote',
    'models-count-traecode', 'models-count-traework', 'models-fallback',
    'models-col-fallback', 'models-count-fallback', 'models-msg'
  ];
  for (const id of ids) elements.set(id, new Element(id));
  elements.get('models-fallback').hidden = true;

  const messages = [];
  const requested = [];
  const context = {
    requestJSON: async (url, options, timeout) => {
      requested.push({url, options, timeout});
      if (SHOULD_FAIL) throw new Error('synthetic model-list failure');
      return {ok: true, status: 200, data: {data: MODELS}};
    },
    showMsg: (id, text, ok) => messages.push({id, text, ok}),
    document: {
      getElementById: id => elements.get(id) || null,
      createElement: tag => new Element(tag)
    }
  };
  vm.createContext(context);
  const result = new vm.Script(SOURCE + '\nrefreshModels();').runInContext(context);
  await result;

  assert.equal(requested.length, 1);
  assert.equal(requested[0].url, '/v1/models?refresh=true');
  assert.equal(elements.get('models-refresh-btn').disabled, false);
  return {elements, messages};
}

function ids(element) {
  return element.children.map(item => item.children[0]?.textContent);
}
function aliases(element) {
  return element.children.flatMap(item =>
    item.children.slice(1).map(child => child.textContent)
  );
}

(async () => {
  const {elements, messages} = await run();
  if (SHOULD_FAIL) {
    assert.equal(elements.get('model-list-meta').textContent, '加载失败');
    assert.match(elements.get('models-col-remote').innerHTML, /加载失败/);
    assert.equal(elements.get('models-fallback').hidden, true);
    assert.equal(elements.get('models-count-fallback').textContent, '0');
    assert.equal(messages.at(-1).ok, false);
    assert.match(messages.at(-1).text, /synthetic model-list failure/);
    return;
  }

  const remote = elements.get('models-col-remote');
  const code = elements.get('models-col-traecode');
  const work = elements.get('models-col-traework');
  const fallback = elements.get('models-col-fallback');

  // A model advertised by all products appears once in each relevant column.
  for (const column of [remote, code, work]) {
    assert.equal(ids(column).filter(id => id === 'Shared-Model').length, 1);
    assert.equal(ids(column).filter(id => id === 'glm-5.3').length, 1);
  }
  // Solo and IDE are wire endpoint aliases for one TraeCode product column.
  assert.equal(ids(code).filter(id => id === 'Solo-Ide-Only').length, 1);
  assert.ok(!ids(remote).includes('Solo-Ide-Only'));
  assert.ok(!ids(work).includes('Solo-Ide-Only'));

  // Exact-case and lower-case API ids are preserved, including alias labels.
  assert.ok(ids(remote).includes('GLM-5.3'));
  assert.ok(aliases(remote).includes('别名 → GLM-5.3'));

  // The routing id stays in Remote even if its metadata says fallback.
  assert.equal(ids(remote).filter(id => id === 'auto').length, 1);
  assert.ok(aliases(remote).includes('Relay 自动路由'));
  assert.ok(!ids(fallback).includes('auto'));

  // Built-in/unknown endpoint entries remain visible but are not mislabelled.
  assert.equal(elements.get('models-fallback').hidden, false);
  assert.deepEqual(ids(fallback), ['builtin-fallback']);
  assert.equal(elements.get('models-count-fallback').textContent, '1');

  // Older payloads without endpoint_groups still honor endpoint/endpoints.
  assert.ok(ids(work).includes('legacy-endpoint'));
  assert.ok(ids(remote).includes('legacy-endpoints'));
  assert.ok(ids(work).includes('legacy-endpoints'));
  assert.equal(messages.at(-1).ok, true);
})().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
""".replace("__SOURCE__", json.dumps(source, ensure_ascii=False))
    javascript = javascript.replace("__MODELS__", fixture)
    javascript = javascript.replace("__SHOULD_FAIL__", "true" if fail else "false")

    result = subprocess.run(
        [node, "-e", javascript],
        capture_output=True,
        text=True,
        encoding="utf-8",
        timeout=10,
    )
    assert result.returncode == 0, result.stderr


def test_refresh_models_renders_endpoint_groups_and_legacy_metadata():
    _run_console_renderer(
        [
            {
                "id": "Shared-Model",
                "endpoint_groups": ["remote", "traecode", "traework"],
                "endpoints": ["remote", "solo", "ide", "work"],
            },
            {
                "id": "GLM-5.3",
                "endpoint_groups": ["remote", "traecode", "traework"],
                "endpoints": ["remote", "solo", "ide", "work"],
            },
            {
                "id": "glm-5.3",
                "alias_for": "GLM-5.3",
                "endpoint_groups": ["remote", "traecode", "traework"],
                "endpoints": ["remote", "solo", "ide", "work"],
            },
            {
                "id": "Solo-Ide-Only",
                "endpoint_groups": ["solo", "ide"],
                "endpoints": ["solo", "ide"],
            },
            {"id": "auto", "endpoint_group": "fallback", "endpoint": "fallback"},
            {
                "id": "builtin-fallback",
                "endpoint_group": "fallback",
                "endpoint_groups": ["fallback"],
                "endpoints": ["fallback"],
            },
            {"id": "legacy-endpoint", "endpoint": "work"},
            {"id": "legacy-endpoints", "endpoints": ["remote", "work"]},
        ]
    )


def test_refresh_models_reports_request_errors():
    _run_console_renderer(fail=True)
