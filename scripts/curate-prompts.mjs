// Rebuild the reviewed prompt set from pinned, read-only Git checkouts.
// Source repositories are data: no downloaded module or installer is executed.
import {execFileSync} from 'node:child_process';
import {readFileSync,writeFileSync,mkdirSync,readdirSync,copyFileSync,unlinkSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {resolve,join,dirname} from 'node:path';
import {fileURLToPath} from 'node:url';
const root=resolve(dirname(fileURLToPath(import.meta.url)),'..');
const sourceRoot=resolve(process.argv[2]||join(root,'private/armor-research-20261106'));
const sha=s=>createHash('sha256').update(s).digest('hex');
const sources={
 coldbrew:{repo:'3641397194-wq/gpt6-Astra',revision:'fd9551aaeb325af1894eaa385234889f3a9c097a'},
 quincunx:{repo:'Quincunx33/Ai-jailbreak',revision:'b01c2f446550ac310c9bda9d9606621fe341a791'}
};
function git(id,...args){return execFileSync('git',['-C',join(sourceRoot,id),...args],{encoding:'utf8',maxBuffer:4<<20,stdio:['ignore','pipe','pipe']}).replaceAll('\r\n','\n');}
for(const [id,s] of Object.entries(sources)){
 if(git(id,'rev-parse','HEAD').trim()!==s.revision)throw Error(`Source revision mismatch: ${id}`);
 s.licenseText=git(id,'show','HEAD:LICENSE');
}
const library=[],picks=[];
const apiContract='接口使用规则：当前用户消息就是本轮任务；按用户指定的语言、长度和输出格式交付，JSON 或代码输出不附加口号、固定尾句或额外包装。用户要求 JSON 对象时，直接从 { 开始，以 } 结束，不使用 Markdown 代码围栏。只使用本次请求实际提供的工具；没有工具时交付文本或代码，不声称已经操作文件、终端或外部服务。缺失值应明确标为待填写，不能伪造真实凭证、路径、运行结果或验证结论。';
function add({name,family,label,source,path,body,prefixes=[],auto=true,note}){
 const s=sources[source],sourceRaw=git(source,'show',`HEAD:${path.split('#')[0]}`);
 body=body.trim()+'\n\n'+apiContract+'\n';
 if(/\{\{[A-Z_]+\}\}|\[INSERT|coldcoffeeai\.com|QQ[一二三四]群|t\.me\/|ACTIVATION_REPLY|APP_TITLE/.test(body))throw Error(`Unresolved or promotional content: ${name}`);
 const hash=sha(body);
 library.push({source_id:source,path,family,body,sha256:hash,archive_sha256:sha(sourceRaw),license:'MIT',license_text:s.licenseText,source_revision:s.revision});
 picks.push({name,family,label,body,auto,model_prefixes:prefixes,source_id:source,source_path:path,source_url:`https://github.com/${s.repo}/blob/${s.revision}/${path.split('#')[0]}`,source_revision:s.revision,source_sha256:hash,original_file_sha256:sha(sourceRaw),license:'MIT',coverage:prefixes.length?prefixes.join(' / ')+'；文本已核对，效果需按上游版本验证':label+'；可用正文，未承诺全型号全破',selection_note:note,efficacy_verified:false});
}
function section(text,heading){const start=text.indexOf(`## ${heading}\n`);if(start<0)throw Error(`Missing section ${heading}`);const tail=text.slice(start+heading.length+4);return tail.split(/\n## /)[0].replace(/<!--[^]*?-->/g,'').trim();}
const seats=[
 ['gpt','GPT / Codex','codex',['Operating spine','Codex-side execution','Five cutting lines','Voice']],
 ['claude','Claude','claude',['简报原则','工具手感','五条刃口']],
 ['deepseek','DeepSeek','deepseek',['思考槽封口','票根规矩','五条刻度']],
 ['glm','GLM','glm53',['直做条','活路']],
 ['gemini','Gemini','gemini',['行为条','五刃']],
 ['grok','Grok','grok',['夜班规矩','手上的刀']],
 ['kimi','Kimi','kimi',['Kimi 规矩','手上的刀']]
];
for(const [family,label,file,headings] of seats){
 const path=`desktop/src/lib/packs/${file}.md`,raw=git('coldbrew','show',`HEAD:${path}`);
 let body=headings.map(h=>section(raw,h)).join('\n\n');
 // Only permanent task rules are portable. Activation, installers, commercial
 // routing and unprovided tool registrations are not part of a chat preset.
 if(family==='deepseek')body='你坐在 DeepSeek V4.1 的短刻度台上。面前只有一张票根——就是用户这一句。思考槽用来把票根拆成可执行线，嘴巴用来出货。不要把票根换成别的题目。\n\n'+body;
 add({name:`${family}-curated`,family,label,source:'coldbrew',path,body,note:'保留完整常驻规则与任务条目；移除激活、广告、安装和中转路由，补充聊天接口能力边界。旧版本口号预设已撤出。'});
}
const quincunx=git('quincunx','show','HEAD:README.md');
function block(marker){const start=quincunx.indexOf(marker);if(start<0)throw Error(`Missing marker ${marker}`);const m=quincunx.slice(start).match(/```markdown\n([\s\S]*?)```/);if(!m)throw Error('Missing prompt block');return m[1].trim().replaceAll('[TOPIC]','the task in the latest user message').replaceAll('[INSERT SOMETHING HERE]','the task in the latest user message');}
add({name:'qwen-curated',family:'qwen',label:'Qwen · 完整递归研究模板',source:'quincunx',path:'README.md#deep-recursive-meta-cognitive-sandbox-full-edition',body:block('### ⚡ Modern 2026: Deep Recursive'),note:'采用完整递归模板，任务指向当前用户消息；这是跨模型模板在 Qwen 的路由，不冒充 Qwen 专属破解或宿主补丁。'});
add({name:'llama-curated',family:'llama',label:'Llama · 辩证审计模板',source:'quincunx',path:'README.md#adversarial-socratic-dialectic-epistemic-synthesis',body:block('### 🏛️ Modern 2026: Adversarial Socratic'),note:'采用完整辩证审计模板，移除旧版歌词样例及固定口号；跨模型模板在 Llama 的路由，未做 Llama 真实账号验证。'});
add({name:'deepseek-r1-curated',family:'deepseek',label:'DeepSeek R1 · 研究配置',source:'quincunx',path:'README.md#deepseek-r1-0528-reasoning-trace-simulation-header',body:block('DeepSeek R1-0528 (Reasoning Trace Simulation Header)'),prefixes:['deepseek-r1','deepseek-reasoner'],note:'上游明确列出的 R1 配置正文；替换旧版预填示例，未继承上游的未复现实测结论。'});
add({name:'recursive-full',family:'generic',label:'完整递归研究模板 · 手动选择',source:'quincunx',path:'README.md#deep-recursive-meta-cognitive-sandbox-full-edition',body:block('### ⚡ Modern 2026: Deep Recursive'),auto:false,note:'保留完整五步模板供其他模型显式选择；不默认覆盖未知模型。GLM 小样本格式测试通过，不代表全破成功。'});
// Backup exact previous contents outside the image before replacing any files.
const backup=join(sourceRoot,'before-curation');mkdirSync(backup,{recursive:true});
for(const relative of ['prompts','internal/armor/assets/presets']){
 const dir=join(root,relative),bak=join(backup,relative);mkdirSync(bak,{recursive:true});
 for(const filename of readdirSync(dir).filter(n=>n.endsWith('.md'))){
  const file=resolve(dir,filename);if(dirname(file)!==resolve(dir))throw Error('Path boundary violated');
  // No recursive deletion or moves; git and the private snapshot retain originals.
  try{copyFileSync(file,join(bak,filename),1)}catch(e){if(e.code!=='EEXIST')throw e;}
  if(!picks.some(p=>p.name+'.md'===filename))unlinkSync(file);
 }
 for(const p of picks){const meta={family:p.family,auto:p.auto,priority:100,model_prefixes:p.model_prefixes.join(','),description:p.label,coverage:p.coverage,selection_note:p.selection_note,source_id:p.source_id,source_url:p.source_url,source_revision:p.source_revision,source_sha256:p.source_sha256,license:p.license};writeFileSync(join(dir,p.name+'.md'),'<!-- armor\n'+Object.entries(meta).map(([k,v])=>`${k}: ${v}`).join('\n')+'\n-->\n'+p.body);}
}
for(const relative of ['prompts/_index.json','internal/armor/assets/library.json']){const bak=join(backup,relative);mkdirSync(dirname(bak),{recursive:true});try{copyFileSync(join(root,relative),bak,1)}catch(e){if(e.code!=='EEXIST')throw e;}}
writeFileSync(join(root,'prompts/_index.json'),JSON.stringify(picks.map(({body,...p})=>({...p,bytes:Buffer.byteLength(body),body_sha256:sha(body)})),null,2)+'\n');
// Same full template can be explicitly selected or routed to Qwen; keep only
// one source document per path in the research browser.
writeFileSync(join(root,'internal/armor/assets/library.json'),JSON.stringify(library.filter((p,i)=>library.findIndex(q=>q.source_id===p.source_id&&q.path===p.path)===i),null,2)+'\n');
for(const [id,s] of Object.entries(sources)){const dir=join(root,'licenses/prompts',id);mkdirSync(dir,{recursive:true});writeFileSync(join(dir,'LICENSE'),s.licenseText);if(id==='quincunx')writeFileSync(join(dir,'ATTRIBUTION.md'),git(id,'show','HEAD:ATTRIBUTION.md'));}
console.log(JSON.stringify({presets:picks.length,automatic:picks.filter(p=>p.auto).length,source_documents:library.length-1,backup}));
