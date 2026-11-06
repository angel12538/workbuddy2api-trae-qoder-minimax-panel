const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const path = require('node:path');
const panel = process.argv[2] || path.join(__dirname, '../internal/panel');
const source = fs.readFileSync(path.join(panel, 'app.js'), 'utf8');
const html = fs.readFileSync(path.join(panel, 'index.html'), 'utf8');
const start = source.indexOf('function clearCaImportInputs()');
const end = source.indexOf('// doCaAdd 拉浏览器', start);
assert(start >= 0 && end > start);
function setup(api) {
  const nodes = Object.fromEntries(['btnCaImportCookie', 'caCookieInput', 'caCookieLabel', 'addCaDone', 'addCaErr'].map(id =>
    [id, {value: '', hidden: true, disabled: false, textContent: id === 'btnCaImportCookie' ? '验证并导入账号' : '', focus() {this.focused = true;}}]));
  const calls = [], state = {refresh: 0, toasts: [], copies: []};
  const ctx = vm.createContext({$: id => nodes[id], api: async (...args) => {calls.push(args); return api(...args);},
    loadCkCodeArts: async () => {state.refresh++; if (state.failRefresh) throw Error('refresh');},
    copyText: async value => {if (state.failCopy) throw Error('clipboard unavailable'); state.copies.push(value);},
    toast: (...args) => state.toasts.push(args), JSON, Error});
  vm.runInContext(source.slice(start, end), ctx);
  return {nodes, calls, state, ctx};
}
(async () => {
  let t = setup(() => {throw Error('should not send');});
  await t.ctx.doCaImportCookie();
  assert.equal(t.calls.length, 0); assert.equal(t.nodes.addCaErr.hidden, false);
  assert.equal(t.nodes.caCookieInput.focused, true);

  for (const [input, expected] of [
    ['POST /furiondataserver/fr?appId=TEST_ONLY HTTP/1.1\nHost: furiondata.myhuaweicloud.com\nAccept: */*', 'furiondata'],
    [':method: POST\n:authority: furiondata.myhuaweicloud.com\n:path: /furiondataserver/fr', 'furiondata'],
    ['GET /chat/home HTTP/1.1\nHost: devcloud.cn-north-4.huaweicloud.com\nAccept: */*', '没有 Cookie'],
    ['GET /chat/home HTTP/1.1\nHost: devcloud.cn-north-4.huaweicloud.com\nCookie: SessionID=TEST_ONLY', '整段请求标头'],
    ['Cookie', 'name=value'],
  ]) {
    t = setup(() => {throw Error('incorrect copied content must not be sent');});
    t.nodes.caCookieInput.value = input;
    await t.ctx.doCaImportCookie();
    assert.equal(t.calls.length, 0);
    assert(t.nodes.addCaErr.textContent.includes(expected));
    assert(!t.nodes.addCaErr.textContent.includes('TEST_ONLY'));
    assert.equal(t.nodes.caCookieInput.value, input);
    assert.equal(t.nodes.caCookieInput.focused, true);
  }
  t = setup(() => ({}));
  await t.ctx.doCaCopyFilter();
  assert.deepEqual(t.state.copies, ['package/overview']);
  t.state.failCopy = true;
  await t.ctx.doCaCopyFilter();
  assert(t.state.toasts.at(-1)[0].includes('手动输入'));

  t = setup(() => ({ok: true, message: '已导入'}));
  t.nodes.caCookieInput.value = ' Cookie: SessionID=TEST_ONLY; a=b== ';
  t.nodes.caCookieLabel.value = ' 我的账号 ';
  await t.ctx.doCaImportCookie();
  assert.equal(t.calls[0][0], 'ide/codearts/import_cookie');
  assert.equal(t.calls[0][1].method, 'POST');
  assert.deepEqual(JSON.parse(t.calls[0][1].body), {cookie: 'Cookie: SessionID=TEST_ONLY; a=b==', label: '我的账号'});
  assert.equal(t.nodes.caCookieInput.value, ''); assert.equal(t.nodes.caCookieLabel.value, '');
  assert.equal(t.nodes.addCaDone.textContent, '已导入'); assert.equal(t.state.refresh, 1);
  assert.equal(t.nodes.btnCaImportCookie.disabled, false);

  for (const result of [{ok: false, message: '会话无效'}, null]) {
    t = setup(() => result); t.nodes.caCookieInput.value = 'a=test';
    await t.ctx.doCaImportCookie();
    assert.equal(t.nodes.caCookieInput.value, 'a=test'); assert.equal(t.state.refresh, 0);
    assert.equal(t.nodes.addCaErr.hidden, false); assert.equal(t.nodes.addCaDone.hidden, true);
  }
  for (const message of ['HTTP 404', '密钥无效或未填写', '网络失败']) {
    t = setup(() => {throw Error(message);}); t.nodes.caCookieInput.value = 'a=test';
    await t.ctx.doCaImportCookie();
    assert.equal(t.nodes.btnCaImportCookie.disabled, false); assert.equal(t.state.refresh, 0);
    assert.equal(t.nodes.caCookieInput.value, 'a=test');
    assert(!t.nodes.addCaErr.textContent.includes('a=test'));
    if (message === 'HTTP 404') assert(t.nodes.addCaErr.textContent.includes('更新服务'));
  }
  let resolve;
  t = setup(() => new Promise(r => {resolve = r;})); t.nodes.caCookieInput.value = 'a=old';
  const pending = t.ctx.doCaImportCookie();
  await t.ctx.doCaImportCookie(); assert.equal(t.calls.length, 1);
  t.nodes.caCookieInput.value = 'a=new'; resolve({ok: true}); await pending;
  assert.equal(t.nodes.caCookieInput.value, 'a=new');

  t = setup(() => ({ok: true})); t.nodes.caCookieInput.value = 'a=test'; t.state.failRefresh = true;
  await t.ctx.doCaImportCookie();
  assert.equal(t.nodes.addCaErr.hidden, true); assert(t.nodes.addCaDone.textContent.includes('刷新失败'));
  t.nodes.caCookieInput.value = 'secret'; t.nodes.caCookieLabel.value = 'test'; t.ctx.clearCaImportInputs();
  assert.equal(t.nodes.caCookieInput.value, ''); assert.equal(t.nodes.caCookieLabel.value, '');
  for (const id of Object.keys(t.nodes)) assert(html.includes(`id="${id}"`), id);
  assert(html.includes('HttpOnly')); assert(html.includes('不能保证永久自动续期'));
  assert(source.includes('function closeAdd() {\n  clearCaImportInputs();'));
  assert(source.includes('function resetCodeArtsAddPanel() {\n  clearCaImportInputs();'));
  console.log('PASS: empty input, request format, success, rejection, auth/network/404 errors, duplicate submit, edited input, refresh failure, cleanup, wrong-request guidance, clipboard fallback, UI wiring');
})().catch(e => {console.error(e); process.exitCode = 1;});
