const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/static/app.js', 'utf8');

function harness(fetchImpl = async () => ({ok:true,json:async()=>({}),text:async()=>''})) {
  class Element {
    constructor(id) { this.id=id; this.listeners={}; this.dataset={};this.value='';this.textContent='';this.innerHTML='';this.disabled=false;this.style={setProperty(){}};const names=new Set(['hidden']);this.classList={add:(...x)=>x.forEach(v=>names.add(v)),remove:(...x)=>x.forEach(v=>names.delete(v)),contains:x=>names.has(x),toggle:(x,on)=>{on ??= !names.has(x);on?names.add(x):names.delete(x);}}; }
    addEventListener(k,f){this.listeners[k]=f} setAttribute(){} querySelector(){return null} appendChild(){} querySelectorAll(){return []} focus(){} reset(){} scrollIntoView(){} contains(){return false}
  }
  const ids = ['auth-panel','auth-form','auth-action','auth-input','auth-username','auth-submit-btn','auth-error','logout-btn','items-list','profile-status-desc','roll-count','stat-queued','btn-apply-update','btn-check-update','update-msg','hero-dial-btn','bulk-read-btn','bulk-archive-btn','sync-btn','flush-btn'];
  const elements = new Map(ids.map(id=>[id,new Element(id)]));
  const pending=new Map();let timerID=0;const storage=new Map(), listeners={};
  const timers={setTimeout:f=>{const id=++timerID;pending.set(id,f);return id},clearTimeout:id=>pending.delete(id),setInterval:()=>0,clearInterval(){}};
  const streams=[];
  class EventSource {constructor(){this.listeners={};streams.push(this)}addEventListener(k,f){this.listeners[k]=f}close(){}}
  const window={...timers,addEventListener:(k,f)=>{listeners[k]=f},requestIdleCallback(){},location:{},innerWidth:1000};
  const document={getElementById:id=>elements.get(id)||null,querySelectorAll:selector=>selector.includes('#hero-dial-btn')?['hero-dial-btn','bulk-read-btn','bulk-archive-btn','sync-btn','flush-btn'].map(id=>elements.get(id)):[],querySelector:()=>null,documentElement:new Element('html'),body:new Element('body'),activeElement:null,createElement:id=>new Element(id)};
  const context=vm.createContext({window,document,navigator:{userAgent:''},location:{search:''},history:{replaceState(){}},localStorage:{getItem:k=>storage.get(k)||null,setItem:(k,v)=>storage.set(k,v),removeItem:k=>storage.delete(k)},fetch:fetchImpl,EventSource,URLSearchParams,URL,console,HTMLInputElement:Element,HTMLTextAreaElement:Element,HTMLSelectElement:Element,HTMLButtonElement:Element,HTMLElement:Element,HTMLCanvasElement:Element,...timers});
  const start=source.indexOf('  window.setInterval(updateNextDigestBadge');
  const script=source.slice(0,start)+`globalThis.testApp = {connectSSE,refresh,checkAuthStatus,finishLogout,clearAccountDisplay,api,updateUserUI,setAuthMode,switchMode,renderProfileDrawer, get authGen(){return authGen}, set authGen(v){authGen=v}}; })();`;
  vm.runInContext(script,context);
  return {app:context.testApp,elements,storage,streams,listeners,pending,async tick(){const entries=[...pending];pending.clear();for(const[,f] of entries)f();await flush();}};
}
const flush=async()=>{for(let i=0;i<8;i++)await Promise.resolve()};
const response=(data,status=200)=>({ok:status>=200&&status<300,status,json:async()=>data,text:async()=>JSON.stringify(data)});

test('event refresh completes before a queued follow-up starts',async()=>{
  const requests=[];const h=harness(async path=>{if(path==='/api/auth/ticket')return response({ticket:'one'});return new Promise(resolve=>requests.push({path,resolve}));});
  h.storage.set('fg_session','alice');h.elements.get('auth-panel').classList.add('hidden');
  await h.app.connectSSE();const event=h.streams[0].listeners.item;
  event();await h.tick();assert.equal(requests.length,2);
  event();event();await h.tick();assert.equal(requests.length,2,'a second event invalidated the running refresh');
  for(const r of requests)r.resolve(response(r.path==='/api/status'?{stats:{mode:'normal',queued_count:1,pushed_today:0,digested_today:0,archived_today:0},heap_mb:1,db_size_kb:1}:{items:[]}));
  await flush();assert.equal(h.elements.get('stat-queued').textContent,'1');
  await h.tick();assert.equal(requests.length,4,'coalesced follow-up did not start');
});

for (const action of ['login', 'register']) test(`stale ${action} cleanup revokes only its explicit session`,async()=>{
  let resolveLogin;const calls=[];const h=harness(async(path,opts)=>{calls.push({path,opts});if(path===`/api/auth/${action}`)return new Promise(resolve=>resolveLogin=resolve);return response({});});
  h.elements.get('auth-input').value='alice-password-123';h.elements.get('auth-username').value='alice';h.elements.get('auth-action').value=action;
  const submit=h.elements.get('auth-form').listeners.submit({preventDefault(){}});await flush();h.app.authGen++;
  resolveLogin(response({token:'obsolete',user:{id:1,username:'alice'}}));await submit;
  const discard=calls.find(c=>c.path==='/api/auth/discard-session');assert.ok(discard);assert.equal(discard.opts.headers.Authorization,'Bearer obsolete');assert.equal(discard.opts.credentials,'omit');assert.ok(!calls.some(c=>c.path==='/api/auth/logout'));
});

test('pending lock survives reload and blocks cookie authentication',async()=>{
  let calls=0;const h=harness(async()=>{calls++;return response({authenticated:true,user:{username:'alice'}})});
  h.storage.set('fg_pending_logout',JSON.stringify({Authorization:'Bearer original'}));
  await h.app.checkAuthStatus();
  assert.equal(calls,0);assert.ok(!h.elements.get('auth-panel').classList.contains('hidden'));
  await h.app.renderProfileDrawer();assert.equal(calls,0);assert.equal(h.elements.get('logout-btn').textContent,'RETRY LOCK');
  h.elements.get('auth-input').value='bob-password-123';h.elements.get('auth-action').value='login';
  await h.elements.get('auth-form').listeners.submit({preventDefault(){}});
  assert.equal(calls,0,'a login replaced the pending revocation');
});

test('account switch clears rendered telemetry even when refresh fails',async()=>{
  const h=harness(async()=>{throw new Error('offline')});
  h.storage.set('fg_session','bob');h.elements.get('stat-queued').textContent='42';h.elements.get('hero-dial-btn').dataset.mode='dnd';
  h.listeners.storage({key:'fg_session'});await flush();
  assert.equal(h.elements.get('stat-queued').textContent,'—');assert.equal(h.elements.get('hero-dial-btn').dataset.mode,undefined);assert.equal(h.elements.get('sync-btn').disabled,true);
});

test('wrong current password preserves authentication while expired session locks',async()=>{
  let status=403;const h=harness(async()=>response({error:'current password incorrect'},status));
  h.storage.set('fg_session','alice');
  await assert.rejects(h.app.api('/api/auth/change-passphrase',{method:'POST'}));assert.equal(h.storage.get('fg_session'),'alice');
  status=401;await assert.rejects(h.app.api('/api/auth/change-passphrase',{method:'POST'}));assert.ok(!h.storage.has('fg_session'));
});

test('committed mode without status triggers refresh without zeroing telemetry',async()=>{
  const calls=[];const h=harness(async path=>{calls.push(path);if(path==='/api/mode')return response({committed:true,warning:'status unavailable'});throw new Error('status unavailable')});
  h.storage.set('fg_session','alice');h.elements.get('auth-panel').classList.add('hidden');h.elements.get('stat-queued').textContent='42';
  await h.app.switchMode('dnd');await flush();
  assert.ok(calls.includes('/api/status'));assert.equal(h.elements.get('stat-queued').textContent,'42');
});

test('failed lock hides account data and retries its original credential',async()=>{
  let success=false;const calls=[];const h=harness(async(path,opts)=>{calls.push({path,opts});return response({},success?200:503)});
  h.storage.set('fg_session','original');h.elements.get('logout-btn').textContent='LOCK APP';h.elements.get('stat-queued').textContent='Alice: 42';
  await h.elements.get('logout-btn').listeners.click({currentTarget:h.elements.get('logout-btn')});
  assert.ok(h.storage.has('fg_pending_logout'));assert.equal(h.elements.get('logout-btn').textContent,'RETRY LOCK');assert.match(h.elements.get('profile-status-desc').textContent,/pending/);assert.equal(h.elements.get('stat-queued').textContent,'—');assert.ok(!h.elements.get('auth-panel').classList.contains('hidden'));
  success=true;await h.elements.get('logout-btn').listeners.click({currentTarget:h.elements.get('logout-btn')});assert.ok(!h.storage.has('fg_pending_logout'));assert.equal(h.elements.get('logout-btn').textContent,'LOCKED');assert.equal(calls.at(-1).opts.headers.Authorization,'Bearer original');
});

test('startup authentication failure exposes a retry without starting inbox requests',async()=>{
  let calls=0;const h=harness(async()=>{calls++;throw new Error('offline')});
  await h.app.checkAuthStatus();assert.ok(!h.elements.get('auth-panel').classList.contains('hidden'));assert.equal(h.streams.length,0);assert.equal(calls,1);
});

for (const outcome of ['success','failure']) test(`cookie-only auth status ignores superseded ${outcome}`,async()=>{
  let resolve,reject;const h=harness(()=>new Promise((yes,no)=>{resolve=yes;reject=no}));
  const pending=h.app.checkAuthStatus();await flush();
  h.app.authGen++;h.elements.get('profile-status-desc').textContent='Newer lock state';
  if(outcome==='success')resolve(response({authenticated:true,user:{username:'alice'}}));else reject(new Error('old request failed'));
  assert.equal(await pending,null);assert.equal(h.elements.get('profile-status-desc').textContent,'Newer lock state');assert.ok(h.elements.get('auth-panel').classList.contains('hidden'));
});

test('update conflict invalidates reviewed identity and disables retry',async()=>{
 const h=harness(async()=>response({error:'release changed'},409));h.elements.get('btn-apply-update').dataset.releaseIdentity='old';
 await h.elements.get('btn-apply-update').listeners.click({currentTarget:h.elements.get('btn-apply-update')});
 assert.equal(h.elements.get('btn-apply-update').dataset.releaseIdentity,undefined);assert.equal(h.elements.get('btn-apply-update').disabled,true);assert.match(h.elements.get('update-msg').textContent,/review the release again/);
});
