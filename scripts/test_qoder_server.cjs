const fs=require('fs'), vm=require('vm'), assert=require('assert');
const src=fs.readFileSync(require('path').join(__dirname,'../internal/panel/app.js'),'utf8');
const start=src.indexOf('async function doQoderSwitch(');
const end=src.indexOf('\nasync function doQoderRemove(',start);
const code=src.slice(start,end);
(async()=>{
 for(const server of [true,false]) {
  let sent,loaded=0,frozen=0,confirmation='';
  const btn={textContent:'切号',disabled:false};
  const ctx={qoderSwitchMode:server?'server':'desktop',confirm:s=>(confirmation=s,true),api:async(_p,r)=>{sent=JSON.parse(r.body);return {switch_mode:server?'server':'desktop',email:'qoder:cn:b'}},toast:()=>{},loadQoderAccounts:async()=>{loaded++},onSwitchDone:()=>{frozen++}};
  vm.createContext(ctx);vm.runInContext(code,ctx);
  await ctx.doQoderSwitch('qoder:cn:b',btn);
  assert.equal(sent.restart,!server);assert.equal(loaded,server?1:0);assert.equal(frozen,server?0:1);
  assert.equal(btn.disabled,false);assert.equal(btn.textContent,'切号');
  assert.equal(confirmation.includes('重新启动'),!server);
 }
 console.log('FRONTEND_SERVER_AND_DESKTOP_SWITCH_PASS');
})();
