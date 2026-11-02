/* i18n 纯查找核（L2）套件 —— 按锚点切出无 DOM 区域，在 node 里直接 eval。
 *
 *   node tests/_test_i18n_core.js
 *
 * 为什么单独测这一段：四级兜底里「查不到就原样返回中文」是整个设计的承重墙
 * （缺翻译不留白、不漏 key），而观察器之所以能安全写 DOM、也正是因为这条
 * 恒等性质成立。它不依赖浏览器，就该有个不依赖浏览器的断言。
 *
 * 切片依赖 dashboard.html 里的 BEGIN-PURE-I18N-CORE / END-PURE-I18N-CORE 锚点；
 * 锚点被改动时 indexOf 会静默给出空串、断言照样「通过」，所以先 require 两枚
 * 锚点都在（dash_extract.js 记的就是这个教训）。
 *
 * 末尾另有一组 schedule 排空的断言：它不属于纯核，但同样只吃 dashboard.html
 * 这一个文件、不需要浏览器，而它守的那条 bug（后台页签 rAF 不触发）只能在
 * 「没有 rAF」的环境里复现 —— 正好是 node。
 */
const fs = require('fs');
const path = require('path');
const ROOT = process.env.QD_DASH_ROOT || path.join(__dirname, '..');
const html = fs.readFileSync(path.join(ROOT, 'dashboard.html'), 'utf8');
const SUITE_MD5 = require('crypto').createHash('md5').update(fs.readFileSync(__filename)).digest('hex');
let PASS = 0, FAIL = 0;
function check(name, cond, extra){
  if(cond){ PASS++; console.log('  ok   ' + name); }
  else { FAIL++; console.log('  FAIL ' + name + (extra !== undefined ? '  ' + JSON.stringify(extra) : '')); }
}

const A0 = '/* BEGIN-PURE-I18N-CORE';
const A1 = '/* END-PURE-I18N-CORE */';
check('纯核锚点存在（缺失会让切片静默变空）', html.indexOf(A0) >= 0 && html.indexOf(A1) > html.indexOf(A0));
const pure = html.slice(html.indexOf(A0), html.indexOf(A1));
check('切出来的区域非空且不含 DOM 引用', pure.length > 800 && !/\bdocument\.|\bwindow\./.test(pure), pure.length);

function fresh(){
  /* 'use strict' 不是可选的：dashboard.html 的 i18n 块本身就声明了严格模式，
   * 而 sloppy eval 会把「给字符串挂属性」这类错误静默吞掉 —— PAT[k].call 那个
   * 真 bug（TypeError 打断整次走查）只有在严格模式下才会暴露。 */
  return new Function('"use strict";' + pure +
    '\nreturn {t:t, load:load, isReady:function(){ return READY; }};')();
}

const PACK = {
  messages: {
    '设置': 'Settings',
    '数据指标看板': 'Analytics',
    '第 1 / 1 页 · 共 0 条记录': 'Page 1 / 1 - 0 records',
  },
  fragments: {
    ' 个 Key 生效': ' keys active',
    '国内版': 'China',
  },
  patterns: {
    '最近一次对话（{0}）走了 {1}：未换区': 'Last call ({0}) used {1}: no switch',
    '{0} 个 Key 生效': '{0} keys active',
    '已启用 {0}，共 {1} 项': 'Enabled {1} items, {0} active',
    '带元字符（+ 号）的 {0}': 'metachar (+) {0}',
  },
};

console.log('[core] 加载与就绪')
let k = fresh();
check('未 load 时 t() 原样返回（就绪前不翻译）', k.t('设置') === '设置' && !k.isReady());
check('空包 load 返回 false 且不进入就绪', fresh().load({}) === false);
check('三层全空的包也不算就绪', fresh().load({messages: {}, patterns: {}, fragments: {}}) === false);
k = fresh();
check('正常包 load 返回 true 且就绪', k.load(PACK) === true && k.isReady());

console.log('[core] tier 1 · 整串精确')
k = fresh(); k.load(PACK);
check('命中标签', k.t('设置') === 'Settings');
check('两端空白被 trim 后仍命中', k.t('  数据指标看板  ') === 'Analytics');

console.log('[core] tier 3 · 拼接片段（同样精确匹配，但排在模式之前）')
check('带前导空格的片段命中', k.t(' 个 Key 生效') === ' keys active');
/* 回归护栏：lookup 曾先 trim 再查表，于是**所有**带空格的片段键永远命不中
 * （' 个 Key 生效' 被 trim 成 '个 Key 生效'）。i18n/README.md 明写「空格属于
 * 键」，这条断言就是那份承诺的可执行版本 —— 拼接出来的文案会缺尾巴。 */
check('片段键的前导空格是键的一部分（不被 trim 掉）',
      k.t(' 个 Key 生效') === ' keys active' && k.t('个 Key 生效') !== ' keys active');
check('尾随空格同理', (function(){
  var z = fresh(); z.load({fragments: {'前缀 ' : 'pre '}});
  return z.t('前缀 ') === 'pre ' && z.t('前缀') === '前缀';
})());
check('独立词片段命中', k.t('国内版') === 'China');

console.log('[core] 折叠空白档（HTML 源码换行+缩进留在 nodeValue 里）')
/* 浏览器只在渲染时折叠空白，TextNode.nodeValue 拿到的是原始缩进。en.json 里有
 * 5 条长提示就是这么抄的：键是折叠过的，源码是折行的。没有这一档它们全部静默失效，
 * 而且顺序是承重的 —— 折叠档必须排在原文/trim 之后，否则空格敏感的片段键被抢走。 */
check('折行缩进的文本命中折叠后的键', (function(){
  var z = fresh(); z.load({messages: {'第一行 第二行': 'Line one two'}});
  return z.t('第一行\n            第二行') === 'Line one two';
})());
check('折叠档不抢空格敏感的片段键', (function(){
  var z = fresh();
  z.load({fragments: {' 个 Key 生效': ' keys active'},
          messages: {'个 Key 生效': 'stolen'}});
  return z.t(' 个 Key 生效') === ' keys active';
})());
check('折叠命中的结果同样满足 t(t(x)) === t(x)', (function(){
  var z = fresh(); z.load({messages: {'第一行 第二行': 'Line one two'}});
  var once = z.t('第一行\n  第二行');
  return once === 'Line one two' && z.t(once) === once;
})());

console.log('[core] tier 2 · 模板与槽位')
check('单槽位命中（槽位里的片段名也一起翻）', k.t('最近一次对话（10:23）走了 国内版：未换区') === 'Last call (10:23) used China: no switch');
check('槽位可重排（中文语序 → 英文语序）',
      k.t('已启用 pt-abc，共 12 项') === 'Enabled 12 items, pt-abc active');
check('槽位内容含正则元字符也不误匹配', k.t('已启用 a.b，共 3 项') === 'Enabled 3 items, a.b active');
check('源里的正则元字符被转义（不被当量词）', k.t('带元字符（+ 号）的 x') === 'metachar (+) x');
check('模式是整串锚定：只是包含该文本不误命中',
      k.t('前缀 已启用 pt-abc，共 12 项 后缀').indexOf('Enabled') === -1);
/* 槽位里往往是另一个可译片段（真实例子：'颜色主题: 跟随系统 (当前浅色)' 的
 * '浅色'，和 '走了 国内版' 的 '国内版'）。整句翻了、槽位留中文 = 半中半英。 */
check('槽位内容递归查表，两层嵌套也能翻到底', (function(){
  var z = fresh();
  z.load({messages: {'浅色': 'Light'},
          patterns: {'主题: {0}': 'Theme: {0}', '跟随 (当前{0})': 'Follow (currently {0})'}});
  return z.t('主题: 跟随 (当前浅色)') === 'Theme: Follow (currently Light)';
})());
/* 源串只写 {0} 的模式编译成 ^(.+)$，会把自己再吃进去一遍。深度上限是唯一
 * 挡住它的东西 —— 不能假设包作者不写这种模式（上限去掉就是 RangeError）。 */
check('恶性自吞模式在深度上限处收敛，输出仍是原串', (function(){
  var z = fresh();
  z.load({patterns: {'{0}': '{0}'}});
  return z.t('任何中文') === '任何中文';
})());

console.log('[core] tier 4 · 兜底恒等（承重墙）')
k = fresh(); k.load(PACK);
const MISS = '当前区官方清单里没有这个模型：它天生只在另一区提供，加账号也改变不了';
check('未收录的中文原样返回，逐字节相等', k.t(MISS) === MISS);
check('未收录的英文也原样返回（反向不翻译）', k.t('Settings') === 'Settings');
check('空串与纯空白原样返回', k.t('') === '' && k.t('   ') === '   ');
check('数字/标点串原样返回', k.t('200') === '200' && k.t('—') === '—');
/* 观察器不会自触发死循环的**唯一**理由就是这条：改写后的文本再走一遍 t()
 * 得到同一个字符串，比较相等就不再写 DOM。它挂了就是无限重绘。 */
check('恒等性质可迭代（t(t(x)) === t(x)，观察器终止性的前提）',
      [MISS, '设置', '国内版', '已启用 a，共 1 项'].every(s => k.t(k.t(s)) === k.t(s)));

console.log('[core] 输入防御')
check('非字符串不抛异常', [null, undefined, 0, 12, {}, [], true].every(v => {
  try { k.t(v); return true; } catch(e){ return false; }
}));
check('非字符串原样返回', k.t(null) === null && k.t(0) === 0);

console.log('[core] 坏包整包拒绝（半套翻译比没翻译更难查）')
function rejects(pack){
  try { fresh().load(pack); return false; } catch(e){ return true; }
}
check('messages 是数组 → 拒绝', rejects({messages: ['a']}));
check('messages 值为数字 → 拒绝', rejects({messages: {'设置': 5}}));
check('messages 值为嵌套对象 → 拒绝', rejects({messages: {'设置': {'nested': 1}}}));
check('messages 值为 null → 拒绝', rejects({messages: {'设置': null}}));
check('patterns 是数组 → 拒绝', rejects({patterns: []}));
check('fragments 类型错 → 拒绝', rejects({fragments: 'nope'}));
check('未知顶层键被忽略（包可以长而无须改核）', fresh().load({messages: {'a': 'b'}, future: {x: 1}}) === true);

console.log('[core] 记忆化不改变结果')
k = fresh(); k.load(PACK);
const once = k.t('设置'), twice = k.t('设置');
check('重复调用同结果', once === twice && once === 'Settings');
const p1 = k.t('最近一次对话（1点）走了 国内版：未换区');
const p2 = k.t('最近一次对话（1点）走了 国内版：未换区');
check('模式命中结果同样稳定', p1 === p2 && p1 === 'Last call (1点) used China: no switch');
check('换包后记忆被清空（不残留旧语言）',
      (k.load({messages: {'设置': 'Einstellungen'}}), k.t('设置') === 'Einstellungen'));
/* 正则编译缓存（PTC）也必须随包作废：它要是留着，换包后匹配用的还是旧表的
 * re，命中的是上一门语言的模板。 */
check('换包后模式按新包重编译（编译缓存不能留旧包的正则）', (function(){
  var z = fresh(); z.load(PACK);
  z.t('已启用 a，共 1 项');
  var ok = z.load({patterns: {'已启用 {0}，共 {1} 项': 'ALT {1}/{0}'}});
  return ok === true && z.t('已启用 a，共 1 项') === 'ALT 1/a';
})());

console.log('[core] schedule 排空 —— 后台页签里 rAF 一帧都不触发（实测 visibilityState=hidden）')
/* 这是浏览器里踩出来的真 bug：只靠 requestAnimationFrame 批次时，页签不可见
 * （切走了、开在后台、被遮挡、headless）→ 排空回调永远不跑 → 整页停在中文，
 * 而且不报错。语言切换本身走 reload，所以「切完立刻切走」是正常用法。 */
const SCHED = html.slice(html.indexOf('function schedule(root){'),
                         html.indexOf('function startObserver'));
check('抽到 schedule 源码（切片静默为空会让下面三条假通过）',
      SCHED.length > 200 && /pending/.test(SCHED), SCHED.length);
check('rAF 之外还有一条定时器兜底路径',
      /requestAnimationFrame\(flush\)/.test(SCHED) && /setTimeout\(flush/.test(SCHED));

function buildSchedule(walk, raf, timer){
  return new Function('READY', 'walk', 'document', 'requestAnimationFrame', 'setTimeout', 'pending',
    '"use strict";' + SCHED + '; return schedule;')(true, walk, {body: 'BODY'}, raf, timer, null);
}
check('rAF 永不回调时仍然走一遍', (function(){
  const w = [];
  buildSchedule(function(r){ w.push(r); }, function(){}, function(f){ f(); })('BODY');
  return w.length === 1 && w[0] === 'BODY';
})());
check('兜底与 rAF 都触发也只排空一次，同批多个根合并成一次遍历', (function(){
  const w = [], jobs = [];
  const s = buildSchedule(function(r){ w.push(r); },
                          function(f){ jobs.push(f); }, function(f){ jobs.push(f); });
  s('A'); s('C');                       // 两次调度 → jobs 里 4 个回调
  jobs.forEach(function(f){ f(); });    // 第一个排空，其余读到 null 直接返回
  return w.join(',') === 'A,C';
})());

console.log('[core] L3 走查 —— 观察器报上来的多数是叶子节点')
/* walk() 的 TreeWalker 用 SHOW_ELEMENT，只会 yield root 的**后代**。文本被 JS
 * 改掉时，观察器给的 target 往往就是那个只有文本子节点的 button/span —— 于是
 * 「属性翻了、正文没翻」，而这条只有真 DOM 才看得见。这里用一个最小 DOM 桩
 * 把 L3 切片跑起来，专测这个形状。 */
const L3 = html.slice(html.indexOf('var WATCH_ATTRS'), html.indexOf('function startObserver'));
check('抽到 L3 走查源码（切片为空会让下面五条假通过）',
      L3.length > 400 && /function walk\(root\)/.test(L3), L3.length);

function txt(v){ return {nodeType: 3, nodeValue: v, parentElement: null}; }
function el(tag, opts){
  opts = opts || {};
  const node = {nodeType: 1, tag: tag, cls: opts.cls || '', attrs: opts.attrs || {},
                childNodes: [], parentElement: null};
  (opts.kids || []).forEach(function(k){
    k.parentElement = node;
    node.childNodes.push(k);
  });
  node.hasAttribute = function(a){ return Object.prototype.hasOwnProperty.call(node.attrs, a); };
  node.getAttribute = function(a){ return node.attrs[a]; };
  node.setAttribute = function(a, v){ node.attrs[a] = v; };
  node.closest = function(sel){
    var parts = sel.split(',').map(function(s){ return s.trim(); });
    for(var n = node; n; n = n.parentElement){
      if(n.nodeType !== 1) continue;
      for(var i = 0; i < parts.length; i++){
        var p = parts[i];
        if(p.charAt(0) === '.' && (' ' + n.cls + ' ').indexOf(' ' + p.slice(1) + ' ') >= 0) return n;
        if(p.charAt(0) !== '.' && n.tag === p) return n;
      }
    }
    return null;
  };
  return node;
}
function descendants(root){
  var out = [];
  (function go(n){ (n.childNodes || []).forEach(function(c){ if(c.nodeType === 1){ out.push(c); go(c); } }); })(root);
  return out;
}
function mkEngine(pack){
  var core = fresh();
  core.load(pack);
  var body = el('body', {});
  var doc = {
    body: body,
    createTreeWalker: function(root){
      var list = descendants(root), i = 0;
      return {nextNode: function(){ return i < list.length ? list[i++] : null; }};
    }
  };
  var build = new Function('t', 'document', '"use strict";' + L3 + '; return {walk: walk};');
  return {walk: build(core.t, doc).walk, body: body, core: core};
}
const LABEL = {'深色': 'Dark', '网关调用量': 'Egress volume', '运行日志': 'Logs'};
(function(){
  /* 叶子根：只有文本子节点的元素，正是观察器最常见的那种 target。 */
  var leaf = el('button', {kids: [txt('深色')]});
  var e = mkEngine({messages: LABEL});
  e.walk(leaf);
  check('叶子元素作为根时，它自己的文本也被翻（观察器报的就是这个形状）',
        leaf.childNodes[0].nodeValue === 'Dark', leaf.childNodes[0].nodeValue);
})();
(function(){
  var span = el('span', {attrs: {title: '深色'}, kids: [txt('网关调用量')]});
  var box = el('div', {kids: [span]});
  var root = el('div', {kids: [box, txt('深色')]});
  var e = mkEngine({messages: LABEL});
  e.walk(root);
  check('整棵子树一起翻：后代文本、后代属性、根直属文本都覆盖',
        span.childNodes[0].nodeValue === 'Egress volume' &&
        span.attrs.title === 'Dark' && root.childNodes[1].nodeValue === 'Dark',
        [span.childNodes[0].nodeValue, span.attrs.title, root.childNodes[1].nodeValue]);
})();
(function(){
  var line = el('div', {cls: 'log-line', kids: [txt('深色')]});
  var codeEl = el('code', {kids: [txt('深色')]});
  var inside = el('span', {kids: [codeEl]});
  var root = el('div', {kids: [line, inside]});
  var e = mkEngine({messages: LABEL});
  e.walk(root);
  check('.log-line 与 <code> 子树仍然不动（日志流和命令保持原样）',
        line.childNodes[0].nodeValue === '深色' &&
        codeEl.childNodes[0].nodeValue === '深色',
        [line.childNodes[0].nodeValue, codeEl.childNodes[0].nodeValue]);
})();
(function(){
  var leaf = el('button', {kids: [txt('深色')]});
  var e = mkEngine({messages: LABEL});
  e.walk(leaf);
  var after = leaf.childNodes[0].nodeValue;
  e.walk(leaf);          // 第二次：seen 记忆 + 恒等兜底，必须原地不动
  check('重复走查不二次改写（观察器终止性在 DOM 侧的同一条）',
        after === 'Dark' && leaf.childNodes[0].nodeValue === 'Dark');
})();
(function(){
  var e = mkEngine({messages: LABEL});
  var ok = true;
  try { e.walk(null); e.walk(txt('深色')); } catch(err){ ok = false; }
  check('walk 接到 null 或文本根也不抛（降级路径）', ok);
})();

console.log('SUITE: ' + path.basename(__filename) + ' md5=' + SUITE_MD5);
console.log('FILE: dashboard.html md5=' + require('crypto').createHash('md5').update(html).digest('hex') +
            ' bytes=' + Buffer.byteLength(html, 'utf8') + ' as-of=' + new Date().toISOString());
console.log('');
console.log('SUMMARY: TOTAL ' + (PASS + FAIL) + ' checks, ' + PASS + ' passed, ' + FAIL + ' failed');
console.log('RESULT: ' + (FAIL ? 'RED' : 'GREEN') + ' (exit ' + (FAIL ? 1 : 0) + ')');
process.exit(FAIL ? 1 : 0);
