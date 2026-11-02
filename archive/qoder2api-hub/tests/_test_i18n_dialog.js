/* PR #114 的原生对话框翻译：函数存在但必须真的装上。
 * 原生 alert/confirm/prompt 不过 DOM，观察器看不到；只定义不调用 = 功能等于没做，
 * 所以这里既测包装本身（只翻第一个字符串参数、透传其余与返回值、只包一次），
 * 也测「确实被装进了语言系统」这条接线。
 */
const fs = require('fs');
const path = require('path');
const ROOT = process.env.QD_DASH_ROOT || path.join(__dirname, '..');
const html = fs.readFileSync(path.join(ROOT, 'dashboard.html'), 'utf8');
let PASS = 0, FAIL = 0;
function check(name, cond, extra){
  if(cond){ PASS++; console.log('  ok   ' + name); }
  else { FAIL++; console.log('  FAIL ' + name + (extra !== undefined ? '  ' + JSON.stringify(extra) : '')); }
}
function slice(from, to){
  const a = html.indexOf(from), b = html.indexOf(to);
  return (a >= 0 && b > a) ? html.slice(a, b) : '';
}

const DIALOG = slice('function installDialogI18n(win, translate){', '/* END-PURE-I18N-CORE */');
check('锚点：installDialogI18n 抽到', DIALOG.length > 400, DIALOG.length);

// 用一个会记录调用的「宿主 window」
function makeWin(){
  const log = [];
  const win = {
    alert: function(m){ log.push(['alert', Array.prototype.slice.call(arguments)]); return undefined; },
    confirm: function(m){ log.push(['confirm', Array.prototype.slice.call(arguments)]); return true; },
    prompt: function(m, d){ log.push(['prompt', Array.prototype.slice.call(arguments)]); return d === undefined ? 'x' : d; },
  };
  return { win: win, log: log };
}
const api = new Function(DIALOG + '; return installDialogI18n;')();
const TABLE = { '确定删除账号?': 'Delete account?', '继续?': 'Continue?' };
const t = s => TABLE[s] || s;

const W = makeWin();
check('安装成功返回 true', api(W.win, t) === true);
W.win.confirm('确定删除账号?');
check('confirm 文案被翻译', W.log[0][1][0] === 'Delete account?', W.log[0]);
check('返回值原样透传（confirm=true）', W.win.confirm('确定删除账号?') === true);

const W2 = makeWin();
api(W2.win, t);
W2.win.prompt('继续?', '默认值');
check('prompt 第二参数不被翻译', W2.log[0][1][1] === '默认值', W2.log[0]);
check('prompt 默认值原样返回', W2.win.prompt('继续?', 'abc') === 'abc');

const W3 = makeWin();
api(W3.win, t);
W3.win.alert(42);
check('非字符串首参不翻（数字原样传）', W3.log[0][1][0] === 42, W3.log[0]);
W3.win.alert();
check('无参调用不炸', W3.log.length === 2);

const W4 = makeWin();
api(W4.win, t);
const before = W4.win.alert;
check('重复安装返回 false', api(W4.win, t) === false);
check('重复安装不二次包装', W4.win.alert === before);
W4.win.alert('继续?');
check('只翻一层（英文不会再被查表）', W4.log[0][1][0] === 'Continue?', W4.log[0]);

check('没有 win 时安全返回 false', api(null, t) === false);
check('translate 不是函数时安全返回 false', api(makeWin().win, null) === false);
const W5 = makeWin();
delete W5.win.alert;
check('宿主缺某个方法时跳过它且仍返回 true', api(W5.win, t) === true && typeof W5.win.confirm === 'function');

// ---- 接线：只定义不调用等于没做 ----
check('导出到 window.QD_I18N.API', /installDialogI18n:\s*installDialogI18n/.test(html));
const CALL = /initLangSystem\s*=\s*function\(\)\{[\s\S]{0,600}?installDialogI18n\(window,\s*t\)/;
check('语言系统启动时真的把它装上（否则功能等于没做）', CALL.test(html));
check('安装点用 window 与全局 t（不是空壳）', html.indexOf('installDialogI18n(window, t)') > 0);

console.log('');
console.log('SUMMARY: TOTAL ' + (PASS + FAIL) + ' checks, ' + PASS + ' passed, ' + FAIL + ' failed');
console.log('RESULT: ' + (FAIL ? 'RED' : 'GREEN') + ' (exit ' + (FAIL ? 1 : 0) + ')');
process.exit(FAIL ? 1 : 0);