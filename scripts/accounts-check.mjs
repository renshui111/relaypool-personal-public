// 验证筛选边界和凭证类型/地区识别，使用合成账号。
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';
import assert from 'node:assert/strict';
const context={window:{},URL};runInNewContext(readFileSync(new URL('../web/src/accounts.js',import.meta.url),'utf8'),context);
const a=context.window.RPAccounts;let checks=0;const check=fn=>{fn();checks++;};
const rows=[{id:'one',name:'国内主号',provider:'workbuddy',region:'cn',enabled:true,has_credential:true,status:'ready',ready:true,models:['glm-5.3']},{id:'two',name:'海外停用',provider:'workbuddy',region:'global',enabled:false,has_credential:true,status:'ready',models:['gpt-test']},{id:'three',name:'缺凭证',provider:'deepseek',region:'cn',enabled:true,has_credential:false,models:[]},{id:'four',name:'冷却',provider:'deepseek',region:'cn',enabled:true,has_credential:true,status:'cooldown',models:['deepseek-test']}];
for(const [options,ids] of [[{query:' GLM-5.3 '},'one'],[{provider:'workbuddy',region:'global'},'two'],[{state:'ready'},'one'],[{state:'disabled'},'two'],[{state:'missing'},'three'],[{state:'cooldown'},'four'],[{query:'不存在'},''],[{},'one,two,three,four']])check(()=>assert.equal(a.filter(rows,options).map(r=>r.id).join(','),ids));
check(()=>assert.equal(a.summary(rows).missing,1));check(()=>assert.equal(a.summary(rows).ready,1));check(()=>assert.equal(a.state({...rows[0],ready:false}),'unverified'));
check(()=>assert.equal(a.credentialKind({api_key:'synthetic'}).auth_type,'api_key'));check(()=>assert.equal(a.credentialKind({OPENAI_API_KEY:'synthetic'}).auth_type,'api_key'));check(()=>assert.equal(a.credentialKind({tokens:{access_token:'synthetic'}}).auth_type,'oauth'));
for(const [input,family,expected] of [[{auth:{domain:'https://www.workbuddy.ai'}},'workbuddy','global'],[{domain:'copilot.tencent.com'},'workbuddy','cn'],[{domain:'codebuddy.cn.evil.example'},'workbuddy',''],[{domain:'www.workbuddy.ai.evil.example'},'workbuddy',''],[{domain:'qoder.sh'},'qoder','global'],[{region:'SG'},'trae-intl','global'],[{domain:'trae.cn'},'trae','cn']])check(()=>assert.equal(a.region(input,family),expected));
check(()=>{a.filter(rows,{sort:'name'});assert.equal(rows.map(r=>r.id).join(','),'one,two,three,four');});
console.log(JSON.stringify({passed:true,checks}));
