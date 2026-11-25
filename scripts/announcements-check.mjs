import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {runInNewContext} from 'node:vm';

const payload=JSON.parse(readFileSync(new URL('../internal/api/announcements.json',import.meta.url),'utf8'));
let focused=0,scrolled=0,opens=0,calls=0;
const returnFocus={isConnected:true,focus(){focused++;}};
const heading={scrollIntoView(){scrolled++;},focus(){focused++;}};
const dialog={open:false,showModal(){this.open=true;opens++;},close(){this.open=false;},querySelector(selector){return selector==='#announcement-history-title'?heading:{focus(){}};}};
const scope={window:{},document:{activeElement:returnFocus,getElementById(){return dialog;}},RP:{baseUrl:()=> 'http://local',key:{value:'test'},announcements:async()=>{calls++;return structuredClone(payload);}}};
runInNewContext(readFileSync(new URL('../web/src/announcements.js',import.meta.url),'utf8'),scope);
const {parse,createVM}=scope.window.RPAnnouncements;
let checks=0;
const check=(name,fn)=>{fn();checks++;console.log('通过：'+name);};
check('最新与历史公告完整读取',()=>{const data=parse(payload);assert.equal(data.release,payload.release);assert.equal(data.history.length,payload.history.length);});
// Fingerprints were captured from each original release before archiving.
check('以往版本逐字归档', () => {
  const archivedHashes = {
    '2026.11.23': 'c9148b210fec3157257b665abf4d7b07f19ae48eb7c371d6612288137ba8f3c1',
    '2026.11.16.3': '72a8c95fa82867c3c12ed5d777a731bc2bc6e22b4ca1dd5ca1bce3d0d9a0e126',
    '2026.11.16.2': '7f4c7945cf1ab67b8dbd2a7e59c7b61a17caf810d1580287a109019a17098d33',
    '2026.11.16.1': 'c801058cc5bc4c582f075389ca4c2a605d6519dbe301d42a201fe527347cacd2',
    '2026.11.16': 'aadc72aeee1119a31d0d4709a8d064babd805693feb21c9ddbf2ec2beb056917',
  };
  for (const [release, hash] of Object.entries(archivedHashes)) {
    const entry = payload.history.find(item => item.release === release);
    assert.ok(entry, release);
    assert.equal(createHash('sha256').update(JSON.stringify(entry)).digest('hex'), hash, release);
  }
});
check('兼容不含历史的旧接口',()=>{const old={...payload};delete old.history;assert.equal(parse(old).history.length,0);});
check('拒绝重复版本与错误历史结构',()=>{assert.throws(()=>parse({...payload,history:[payload]}));assert.throws(()=>parse({...payload,history:{}}));assert.throws(()=>parse({...payload,history:[null]}));});
check('文字作为纯文本保留且不透传额外字段',()=>{const p={...payload,title:'<img src=x onerror=alert(1)>',unexpected:'value'};const got=parse(p);assert.equal(got.title,p.title);assert.equal(got.unexpected,undefined);});
const vm=createVM(x=>x,async()=>{});
await vm.openOnEntry();await vm.openOnEntry();
check('进入一次只打开最新公告一次',()=>{assert.equal(opens,1);assert.equal(calls,1);assert.equal(vm.data.release,payload.release);});
vm.showHistory();check('历史入口滚动并聚焦到历史标题',()=>{assert.equal(scrolled,1);assert.equal(focused,1);});
vm.close();vm.onClosed();check('关闭恢复焦点且停止加载',()=>{assert.equal(dialog.open,false);assert.equal(focused,2);assert.equal(vm.loading,false);});
await vm.show();check('重新打开仍可查看历史',()=>{assert.equal(vm.data.history.length,payload.history.length);assert.equal(dialog.open,true);});
let resolveOld,started;
const fetched=new Promise(resolve=>started=resolve);
scope.RP.announcements=()=>new Promise(resolve=>{resolveOld=resolve;started();});
const pending=vm.show();await fetched;vm.reset();resolveOld(payload);await pending;
check('重置后旧响应不会恢复历史或打开弹窗',()=>{assert.equal(vm.data.history.length,0);assert.equal(dialog.open,false);});
scope.RP.announcements=async()=>{throw Error('offline');};await vm.show();
check('读取失败可关闭并重试',()=>{assert.match(vm.error,/offline/);assert.equal(vm.loading,false);vm.close();vm.onClosed();});
scope.RP.announcements=async()=>payload;await vm.show();
check('重试后恢复全部历史',()=>{assert.equal(vm.error,'');assert.equal(vm.data.history.length,payload.history.length);});
console.log(JSON.stringify({passed:true,checks}));
