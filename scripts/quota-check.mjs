// 验证余额显示边界，不用本地调用日志推算上游余额。
import {readFileSync} from 'node:fs';
import {runInNewContext} from 'node:vm';
import assert from 'node:assert/strict';
const context={window:{}};
runInNewContext(readFileSync(new URL('../web/src/quota.js',import.meta.url),'utf8'),context);
const q=context.window.RPQuota;
let passed=0;
function check(fn){fn();passed++;}
for(const v of [undefined,null,-1,NaN,Infinity,'0',{},false])check(()=>assert.equal(q.format(v),'未知'));
check(()=>assert.equal(q.format(0),'0'));
check(()=>assert.equal(q.label(null),'未查询'));
for(const [state,label] of [['unsupported','未接入查询'],['unknown','额度未知'],['error','查询失败'],['loading','查询中…']])check(()=>assert.equal(q.label({state}),label));
check(()=>assert.equal(q.label({state:'ok',amounts:[{unit:'积分',remaining:0}]}),'0 积分'));
check(()=>assert.equal(q.label({state:'ok',amounts:[{unit:'积分'}]}),'额度未知'));
check(()=>assert.equal(q.label({state:'ok',amounts:[{unit:'CNY',remaining:12},{unit:'USD',remaining:4}]}),'12 CNY / 4 USD'));
console.log(JSON.stringify({passed:true,checks:passed}));
