#!/usr/bin/env python3
"""Extract untranslated i18n candidates from dashboard.html.

This is a developer aid, not part of the gateway runtime. It runs a small
JavaScript scanner (Node.js) that walks dashboard.html's HTML text runs and
JavaScript string literals / concatenations and reports the Chinese strings
that are not yet covered by i18n/en.json.

Usage:
    python scripts/extract_i18n_keys.py
    python scripts/extract_i18n_keys.py --out ./i18n-scan-out
    python scripts/extract_i18n_keys.py --context 1-40
    python scripts/extract_i18n_keys.py --json

Requirements:
    * Node.js on PATH (available in CI and most dev setups).
    * The 'acorn' npm package (the scanner uses it for JS AST parsing).
      Install it with:  npm install acorn   (in scripts/ dir or project root)
      or globally:  npm install -g acorn
"""

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

SCAN_JS = r'''
const fs = require('fs');
const acorn = require('acorn');
const ROOT = process.env.I18N_ROOT;
const dash = fs.readFileSync(ROOT + '/dashboard.html', 'utf8');
const pack = JSON.parse(fs.readFileSync(ROOT + '/i18n/en.json', 'utf8'));
const MSG = pack.messages, FRG = pack.fragments, PAT = pack.patterns;
const CJK = /[\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff]/;

function squash(s) { return s.replace(/\s+/g, ' ').trim(); }
function lookup(s) {
  if (Object.prototype.hasOwnProperty.call(MSG, s)) return true;
  if (Object.prototype.hasOwnProperty.call(FRG, s)) return true;
  const t = s.trim();
  if (t !== s) {
    if (Object.prototype.hasOwnProperty.call(MSG, t)) return true;
    if (Object.prototype.hasOwnProperty.call(FRG, t)) return true;
  }
  const q = squash(s);
  if (q !== t) {
    if (Object.prototype.hasOwnProperty.call(MSG, q)) return true;
    if (Object.prototype.hasOwnProperty.call(FRG, q)) return true;
  }
  return false;
}
function compile(tpl) {
  let slots = [];
  let re = tpl.replace(/[.*+?^$()|[\]\\]/g, '\\$&')
    .replace(/\{(\d+)\}/g, (_, i) => { slots.push(+i); return '([\\s\\S]+?)'; });
  return { re: new RegExp('^' + re + '$'), slots };
}
const pats = Object.keys(PAT).map(k => ({ k, c: compile(k) }));
function patternHit(s) {
  const t = s.trim();
  for (const p of pats) if (p.c.re.test(t)) return p.k;
  return null;
}
function htmlTextRuns(s) {
  s = s.replace(/<script[\s\S]*?<\/script>/gi, ' ')
       .replace(/<style[\s\S]*?<\/style>/gi, ' ');
  const out = [];
  for (let p of s.split(/<[^>]*>/)) {
    p = p.replace(/&nbsp;/g, ' ').replace(/&quot;/g, '"')
         .replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>');
    if (CJK.test(p) && p.trim()) out.push(p);
  }
  return out;
}
function attrs(s) {
  const out = [];
  const re = /\b(title|placeholder|aria-label)\s*=\s*"([^"]*)"/g;
  let m;
  while ((m = re.exec(s))) if (CJK.test(m[2])) out.push(m[2]);
  return out;
}
function contextOf(src, node) {
  const start = Math.max(0, node.start - 120);
  const end = Math.min(src.length, node.end + 120);
  return src.slice(start, end).replace(/\s+/g, ' ').trim();
}
const scriptRe = /<script[^>]*>([\s\S]*?)<\/script>/gi;
let sm;
const messages = new Map();
const patterns = new Map();
while ((sm = scriptRe.exec(dash))) {
  const src = sm[1];
  const start = sm.index + sm[0].indexOf('>') + 1;
  const lineOffset = dash.slice(0, start).split('\n').length - 1;
  let ast;
  try {
    ast = acorn.parse(src, {
      ecmaVersion: 2022, locations: true,
      allowReturnOutsideFunction: true,
      allowAwaitOutsideFunction: true,
      allowHashBang: true,
    });
  } catch (e) {
    process.stderr.write('PARSE FAIL: ' + e.message + '\n');
    continue;
  }
  function addMessage(s, node, kind) {
    for (const r of htmlTextRuns(s)) {
      if (lookup(r)) continue;
      if (!messages.has(r)) {
        messages.set(r, { s: r, line: lineOffset + node.loc.start.line,
          kind: kind + '-text', ctx: contextOf(src, node) });
      }
    }
    for (const a of attrs(s)) {
      if (lookup(a)) continue;
      if (!messages.has(a)) {
        messages.set(a, { s: a, line: lineOffset + node.loc.start.line,
          kind: kind + '-attr', ctx: contextOf(src, node) });
      }
    }
  }
  function flatten(node) {
    if (node.type === 'BinaryExpression' && node.operator === '+') {
      return flatten(node.left).concat(flatten(node.right));
    }
    if (node.type === 'Literal' && typeof node.value === 'string') return [{ s: node.value }];
    if (node.type === 'TemplateLiteral') {
      const out = [];
      for (let i = 0; i < node.quasis.length; i++) {
        out.push({ s: node.quasis[i].value.cooked || '' });
        if (i < node.expressions.length) out.push({ d: node.expressions[i] });
      }
      return out;
    }
    return [{ d: node }];
  }
  function processPhrase(node) {
    const parts = flatten(node);
    if (parts.length === 1 && parts[0].s !== undefined) {
      addMessage(parts[0].s, node, 'literal');
      return;
    }
    if (!parts.some(p => p.s !== undefined && CJK.test(p.s))) return;
    let slot = 0;
    let out = '';
    for (const p of parts) {
      if (p.s !== undefined) out += p.s;
      else out += '{' + (slot++) + '}';
    }
    for (const r of htmlTextRuns(out)) {
      if (lookup(r) || patternHit(r)) continue;
      if (!patterns.has(r)) {
        patterns.set(r, { s: r, line: lineOffset + node.loc.start.line,
          kind: 'expr', ctx: contextOf(src, node) });
      }
    }
    for (const a of attrs(out)) {
      if (lookup(a) || patternHit(a)) continue;
      if (!patterns.has(a)) {
        patterns.set(a, { s: a, line: lineOffset + node.loc.start.line,
          kind: 'expr-attr', ctx: contextOf(src, node) });
      }
    }
  }
  const seen = new WeakSet();
  function walk(node, parent) {
    if (!node || typeof node !== 'object') return;
    if (node.type === 'BinaryExpression' && node.operator === '+') {
      if (!parent || !(parent.type === 'BinaryExpression' && parent.operator === '+')) {
        processPhrase(node);
      }
      return;
    }
    if (node.type === 'TemplateLiteral') processPhrase(node);
    if (node.type === 'Literal' && typeof node.value === 'string') {
      addMessage(node.value, node, 'literal');
    }
    for (const key of Object.keys(node)) {
      if (key === 'loc' || key === 'start' || key === 'end' || key === 'range' || key === 'type') continue;
      const v = node[key];
      if (Array.isArray(v)) {
        for (const c of v) if (c && typeof c === 'object') walk(c, node);
      } else if (v && typeof v === 'object' && v.type) {
        walk(v, node);
      }
    }
  }
  walk(ast, null);
}
process.stdout.write(JSON.stringify({
  messages: [...messages.values()],
  patterns: [...patterns.values()],
}));
'''


def find_node():
    return shutil.which('node') or shutil.which('node.exe')


def find_global_node_modules():
    """Get global node_modules path via npm root -g."""
    npm = shutil.which('npm') or shutil.which('npm.cmd')
    if not npm:
        return None
    try:
        result = subprocess.run(
            [npm, 'root', '-g'],
            capture_output=True,
            text=True,
            encoding='utf-8',
            timeout=5
        )
        if result.returncode == 0 and result.stdout.strip():
            return Path(result.stdout.strip())
    except Exception:
        pass
    return None


def find_acorn_root(root):
    """Find directory containing acorn in node_modules."""
    candidates = [
        Path(__file__).resolve().parent,  # scripts/
        root,  # project root
    ]
    
    # Check local paths first
    for c in candidates:
        if (c / 'node_modules' / 'acorn' / 'package.json').is_file():
            return c
    
    # Check global node_modules
    global_nm = find_global_node_modules()
    if global_nm and (global_nm / 'acorn' / 'package.json').is_file():
        return global_nm.parent  # return parent so NODE_PATH works
    
    return None


def run_scan(root):
    node = find_node()
    if not node:
        raise SystemExit('error: node not found on PATH; install Node.js to use this scanner')
    acorn_root = find_acorn_root(root)
    if acorn_root is None:
        raise SystemExit(
            'error: acorn package not found. Install it with:\n'
            '    npm install acorn          (local)\n'
            '    npm install -g acorn       (global)'
        )
    env = dict(os.environ)
    env['I18N_ROOT'] = str(root)
    # Put the acorn-bearing directory first on NODE_PATH.
    env['NODE_PATH'] = str(acorn_root / 'node_modules') + os.pathsep + env.get('NODE_PATH', '')
    with tempfile.NamedTemporaryFile('w', suffix='.js', delete=False, encoding='utf-8') as fh:
        fh.write(SCAN_JS)
        js_path = fh.name
    try:
        proc = subprocess.run(
            [node, js_path],
            env=env,
            capture_output=True,
            text=True,
            encoding='utf-8',
        )
    finally:
        try:
            os.unlink(js_path)
        except OSError:
            pass
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr)
        raise SystemExit('error: scanner failed (exit %d)' % proc.returncode)
    if not proc.stdout.strip():
        raise SystemExit('error: scanner produced no output')
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        sys.stderr.write(proc.stdout[:500] + '\n')
        raise SystemExit('error: could not parse scanner output: ' + str(exc))


def parse_range(text):
    if '-' not in text:
        raise argparse.ArgumentTypeError('expected START-END, e.g. 1-40')
    a, b = text.split('-', 1)
    try:
        lo, hi = int(a), int(b)
    except ValueError:
        raise argparse.ArgumentTypeError('START and END must be integers')
    if lo < 1 or hi < lo:
        raise argparse.ArgumentTypeError('invalid range')
    return lo, hi


def main():
    try:
        sys.stdout.reconfigure(encoding='utf-8')
        sys.stderr.reconfigure(encoding='utf-8')
    except Exception:
        pass
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', default=None,
                        help='repo root (default: parent of scripts/)')
    parser.add_argument('--out', default=None,
                        help='directory to write candidates.json / candidates.txt '
                             '(default: i18n/)')
    parser.add_argument('--context', type=parse_range, default=None,
                        help='print items in range START-END with context, then exit')
    parser.add_argument('--json', action='store_true',
                        help='print the full candidate JSON to stdout')
    args = parser.parse_args()

    here = Path(__file__).resolve().parent
    root = Path(args.root).resolve() if args.root else here.parent
    if not (root / 'dashboard.html').is_file() or not (root / 'i18n' / 'en.json').is_file():
        raise SystemExit('error: %s does not look like the qoder2api-hub repo root' % root)

    data = run_scan(root)
    msgs = data.get('messages', [])
    pats = data.get('patterns', [])

    if args.json:
        print(json.dumps(data, ensure_ascii=False, indent=2))
        return

    if args.context:
        lo, hi = args.context
        items = msgs + pats
        for i, item in enumerate(items, 1):
            if lo <= i <= hi:
                print('%3d L%-5d %s' % (i, item.get('line', 0), item.get('s', '').replace('\n', '\\n')))
                ctx = item.get('ctx', '')
                if ctx:
                    print('    ' + ctx[:230])
        print('\n(%d of %d items shown)' % (min(hi, len(items)) - lo + 1, len(items)))
        return

    out_dir = Path(args.out).resolve() if args.out else root / 'i18n'
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / 'candidates.json').write_text(
        json.dumps(data, ensure_ascii=False, indent=2), encoding='utf-8')
    with (out_dir / 'candidates.txt').open('w', encoding='utf-8') as fh:
        fh.write('# messages: %d\n' % len(msgs))
        for m in msgs:
            fh.write('M  L%-5d %s\n' % (m.get('line', 0), m.get('s', '')))
        fh.write('# patterns: %d\n' % len(pats))
        for p in pats:
            fh.write('P  L%-5d %s\n' % (p.get('line', 0), p.get('s', '')))

    print('messages: %d' % len(msgs))
    print('patterns: %d' % len(pats))
    print('written: %s' % out_dir)


if __name__ == '__main__':
    main()
