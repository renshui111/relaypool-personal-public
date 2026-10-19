// Read-only browser/API smoke check for a local personal-edition deployment.
// Set RELAYPOOL_SMOKE_KEY_FILE to a local file containing the management key.
import {spawn} from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import {tmpdir} from 'node:os';
import assert from 'node:assert/strict';
const base=process.env.RELAYPOOL_SMOKE_BASE||'http://127.0.0.1:8788';
assert(['127.0.0.1','localhost'].includes(new URL(base).hostname));
const key=fs.readFileSync(process.env.RELAYPOOL_SMOKE_KEY_FILE,'utf8').trim();
const headers={Authorization:'Bearer '+key};
let checks=0;
const check=(ok,label)=>{assert(ok,label);checks++;};
for(const route of ['/healthz','/readyz','/admin/accounts','/admin/keys','/admin/stats','/v1/models','/console/']){
 const r=await fetch(base+route,{headers});check(r.status===200,route);
}
check((await fetch(base+'/admin/accounts')).status===401,'authentication still required');
for(const route of ['/user/','/user/api/register','/admin/users','/admin/commerce','/payments/epay/notify','/admin/network','/admin/network/tunnel','/console/user/','/console/users.js','/console/commerce.js','/console/network.js']){
 check((await fetch(base+route,{headers})).status===404,'removed '+route);
}
const browser=process.env.RELAYPOOL_BROWSER||'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
const profile=fs.mkdtempSync(path.join(tmpdir(),'relaypool-personal-smoke-'));
let stderr='';
const child=spawn(browser,['--headless=new','--disable-gpu','--no-first-run','--no-default-browser-check','--remote-debugging-port=0','--user-data-dir='+profile,'about:blank'],{stdio:['ignore','ignore','pipe'],windowsHide:true});
child.stderr.on('data',d=>stderr+=d.toString());
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
let ws;
try{
 let match;for(let i=0;i<100;i++){match=stderr.match(/DevTools listening on (ws:\/\/[^\s]+)/);if(match)break;await sleep(100);}
 assert(match,'browser debugger did not start');
 const debugOrigin=new URL(match[1]);
 const tabs=await(await fetch('http://'+debugOrigin.host+'/json/list')).json();
 ws=new WebSocket(tabs.find(t=>t.type==='page').webSocketDebuggerUrl);
 await new Promise((res,rej)=>{ws.addEventListener('open',res,{once:true});ws.addEventListener('error',rej,{once:true});});
 let id=0;const pending=new Map(),errors=[];
 ws.addEventListener('message',e=>{const m=JSON.parse(e.data);if(m.id&&pending.has(m.id)){const p=pending.get(m.id);pending.delete(m.id);clearTimeout(p.timer);m.error?p.reject(Error(m.error.message)):p.resolve(m.result);}if(m.method==='Runtime.exceptionThrown')errors.push(m.params.exceptionDetails.text);});
 const send=(method,params={})=>new Promise((resolve,reject)=>{const i=++id;const timer=setTimeout(()=>{pending.delete(i);reject(Error('CDP timeout '+method));},15000);pending.set(i,{resolve,reject,timer});ws.send(JSON.stringify({id:i,method,params}));});
 const evaluate=async expression=>{const r=await send('Runtime.evaluate',{expression,returnByValue:true,awaitPromise:true});if(r.exceptionDetails)throw Error(r.exceptionDetails.text);return r.result.value;};
 await send('Runtime.enable');
 await send('Page.navigate',{url:base+'/console/'});
 for(let i=0;i<100;i++){if(await evaluate('Boolean(window.RPAPP?.gate)'))break;await sleep(100);}
 await evaluate(`window.RPAPP.gate.keyInput=${JSON.stringify(key)}; window.RPAPP.gate.submit()`);
 for(let i=0;i<100;i++){if(await evaluate('Boolean(window.RPAPP?.authed && window.RPAPP?.overview)'))break;await sleep(100);}
 check(await evaluate('Boolean(window.RPAPP?.authed)'),'browser login: '+await evaluate('JSON.stringify({hasApp:!!window.RPAPP,error:window.RPAPP?.conn?.connError,title:document.title})')+' '+errors.join('; '));
 for(const route of ['overview','accounts','chat','armor','logs','keys','settings']){
  await evaluate(`location.hash='#/${route}'`);
  let loaded=false;for(let i=0;i<100;i++){loaded=await evaluate(`window.RPAPP?.route===${JSON.stringify(route)} && !!document.querySelector('.route.${route==='accounts'?'accounts-page':route==='overview'?'overview-page':route==='chat'?'chat-route':route==='logs'?'logs-page':route==='keys'?'keys-page':route==='settings'?'settings-page':'route'}')`);if(loaded)break;await sleep(100);}
  check(loaded,'browser route '+route);
 }
 check(await evaluate(`!document.querySelector('a[href="#/users"],a[href="#/commerce"],a[href="#/network"]')`),'removed navigation');
 for(const route of ['users','commerce','network']){await evaluate(`location.hash='#/${route}'`);await sleep(300);check(await evaluate(`window.RPAPP.route==='overview'`),'removed browser route '+route);}
 check(errors.length===0,'no browser exceptions: '+errors.join('; '));
 console.log(JSON.stringify({passed:true,checks,real_upstream_chat_called:false}));
}finally{
 if(ws)ws.close();child.kill();
 // Only remove the unique temporary profile allocated by this script.
 await sleep(500);
 if(path.dirname(profile)===path.resolve(tmpdir())&&path.basename(profile).startsWith('relaypool-personal-smoke-'))fs.rmSync(profile,{recursive:true,force:true,maxRetries:5,retryDelay:200});
}
