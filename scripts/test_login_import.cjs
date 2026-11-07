const fs = require('fs'), vm = require('vm'), assert = require('assert/strict');
const path = require('path');
const panel = process.argv[2] || path.join(__dirname, '../internal/panel');
const code = fs.readFileSync(path.join(panel, 'app.js'), 'utf8');
const html = fs.readFileSync(path.join(panel, 'index.html'), 'utf8');
function body(name) {
  const start = code.search(new RegExp('(?:async )?function ' + name + '\\('));
  assert(start >= 0, name);
  // Function boundaries in this file are at column zero.
  const lineEnd = code.indexOf('\n', start);
  const inlineEnd = code.lastIndexOf('}', lineEnd);
  const end = inlineEnd > start ? inlineEnd + 1 : code.indexOf('\n}', start) + 2;
  return code.slice(start, end);
}
const elements = new Map([...html.matchAll(/id="([^"]+)"/g)].map(m => [m[1], {hidden: false, value: '', textContent: '', disabled: false, focus() {this.focused = true;}}]));
const $ = id => { assert(elements.has(id), 'Missing element ' + id); return elements.get(id); };
let calls = [], responder = async () => { throw new Error('TEST: browser unavailable'); }, timer;
const context = { $, URL, encodeURIComponent, JSON, console,
  api: async (path, opts) => { calls.push({path, opts}); return responder(path, opts); },
  setTimeout: f => {timer = f; return 1;}, clearTimeout: () => {},
  loadQoderAccounts() {}, loadCkQoder() {}, loadCkDoubao() {} };
vm.createContext(context);
vm.runInContext(`let qoderWebTimer=null,qoderLoginTimer=null,dbBrowserTimer=null,qoderDeviceTimer=null,qoderDeviceState='',qoderDeviceGeneration=0;\n` +
  ['stopQoderWebPoll','stopQoderLoginPoll','stopDbBrowserPoll','doQoderWebLogin','doQoderLogin','doDbBrowserAdd','resetDoubaoAddPanel','applyLoginCapabilities','stopQoderDevicePoll','startQoderDeviceLogin','pollQoderDevice','doDbAddCookie'].map(body).join('\n'), context);
(async () => {
  for (const [func, box, err] of [['doQoderWebLogin','addQoderWebBox','addQoderWebErr'], ['doQoderLogin','addQoderLoginBox','addQoderLoginErr'], ['doDbBrowserAdd','addDbBrowserBox','addDbBrowserErr']]) {
    await context[func]('test@example.com');
    assert.equal($(box).hidden, false); assert.equal($(err).hidden, false);
    assert.match($(err).textContent, /browser unavailable/);
  }
  responder = async () => ({desktop_login: false});
  await context.applyLoginCapabilities();
  assert.equal($('qoderDesktopOptions').hidden, true); assert.equal($('doubaoDesktopOptions').hidden, true);
  context.resetDoubaoAddPanel(); assert.equal($('addDbManual').hidden, false);
  $('dbCookieInput').value = '  '; calls = [];
  await context.doDbAddCookie(); assert.equal(calls.length, 0); assert.equal($('addDbErr').hidden, false);
  $('dbCookieInput').value = 'sessionid=TEST_ONLY; ttwid=TEST_ONLY';
  responder = async () => ({msg:'Saved'});
  await context.doDbAddCookie(); assert.equal($('dbCookieInput').value, '');
  assert.equal($('addDbDone').hidden, false);
  $('qdDeviceRegion').value = 'cn';
  responder = async path => path.endsWith('/start') ? {state:'test-state',auth_url:'https://qoder.com.cn/device/selectAccounts?challenge=test'} : {done:false,message:'Waiting'};
  await context.startQoderDeviceLogin();
  assert.equal($('qdDeviceUrl').hidden, false); assert.equal($('btnQoderDevice').disabled, false);
  assert.equal(JSON.parse(calls.at(-1).opts.body).region, 'cn');
  await context.pollQoderDevice(); assert.equal($('qdDeviceErr').hidden, true);
  responder = async () => {throw new Error('TEST: network error');};
  await context.pollQoderDevice(); assert.equal($('qdDeviceErr').hidden, false); assert.equal($('btnQoderDeviceRetry').hidden, false);
  responder = async () => ({done:true,ok:true,message:'Added'});
  await context.pollQoderDevice(); assert.equal($('addQoderDone').hidden, false); assert.equal($('qdDeviceUrl').hidden, true);
  responder = async () => ({state:'test-state',auth_url:'https://example.invalid/device/selectAccounts'});
  await context.startQoderDeviceLogin(); assert.equal($('qdDeviceUrl').hidden, true); assert.equal($('qdDeviceErr').hidden, false);
  let finish;
  responder = () => new Promise(resolve => finish = resolve);
  const pending = context.startQoderDeviceLogin(); context.stopQoderDevicePoll();
  finish({state:'stale',auth_url:'https://qoder.com.cn/device/selectAccounts'}); await pending;
  assert.equal($('qdDeviceUrl').hidden, true);
  console.log('PASS: visible errors, NAS choices, cookie import, device login, retry, URL validation and stale responses');
})().catch(e => {console.error(e); process.exit(1);});
