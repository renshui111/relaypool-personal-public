// 概览只查询现有管理接口；刷新不会发送聊天、领奖或修改账号。
(function () {
  "use strict";
  const number = value => Number.isFinite(Number(value)) ? Math.max(0, Number(value)) : 0;
  const states = [{key:"ready",label:"可调度",color:"#16876a"},{key:"cooldown",label:"冷却中",color:"#bb821d"},{key:"offline",label:"需要配置",color:"#d45b62"},{key:"disabled",label:"已停用",color:"#a9b3c5"}];
  function accountState(a) {
    if(!a.enabled)return "disabled";
    if(!a.has_credential)return "offline";
    if(a.status==="cooldown")return "cooldown";
    if(a.ready===false || a.status==="not_ready")return "offline";
    return "ready";
  }
  function localLabel(start) {
    const date=new Date(start);
    return Number.isFinite(date.getTime()) ? date.toLocaleString("zh-CN",{month:"2-digit",day:"2-digit",hour:"2-digit",minute:"2-digit",hour12:false}) : "—";
  }
  const providerNames={workbuddy:"WorkBuddy",qoder:"Qoder",openai:"OpenAI",claude:"Claude",gemini:"Gemini",deepseek:"DeepSeek",codex:"Codex","trae-intl":"Trae 国际",trae:"Trae 国内","openai-compatible":"兼容 API",unattributed:"未分配渠道"};
  const refreshPreference="relaypool.overview.autoRefresh";
  function readRefreshPreference(){try{return window.localStorage.getItem(refreshPreference)!=="false";}catch{return true;}}
  window.RPOverview={createVM(reactive){
    // 计数和图表均来自实际响应，加载失败保留旧数据并明确标记。
    const vm=reactive({
      loading:false,refreshing:false,error:"",lastUpdated:null,autoRefresh:readRefreshPreference(),version:null,ready:null,accounts:[],armor:null,keyArmor:null,schedule:null,recent:[],sectionErrors:{},
      window:"24h",stats:null,statsError:"",statsLoading:false,chartMode:"requests",chartIndex:null,
      get counts(){
        const c={total:this.accounts.length,enabled:0,ready:0,cooldown:0,offline:0,disabled:0,models:0,inflight:0,verified:0};
        const models=new Set();
        for(const a of this.accounts){c[accountState(a)]++;if(a.enabled){c.enabled++;c.inflight+=number(a.in_flight);if(a.ready===true)c.verified++;for(const m of a.models||[])if(m)models.add(m);}}
        c.models=models.size;return c;
      },
      get summary(){return this.stats?.summary || {};},
      get retentionNote(){
        const days=this.stats?.retention_days;
        let note=Number.isInteger(days)?days>0?'日志保留 '+days+' 天，较早记录会清理。':'自动清理已关闭，仍只统计现有日志。':'保留策略尚未读取，统计仅来自现有日志。';
        if(this.stats?.logging_enabled===false)note='请求日志记录已关闭；'+note;
        if(days>0&&new Date(this.stats.until)-new Date(this.stats.since)>days*86400000)note+='所选窗口超过保留期，统计可能不完整。';
        return note+' 不属于永久账本。';
      },
      get failedSections(){const names={version:"版本信息",ready:"服务状态",armor:"预设目录",keyArmor:"本密钥破甲权限",schedule:"领取计划",recent:"最近调用"};return Object.keys(names).filter(key=>this.sectionErrors[key]).map(key=>names[key]);},
      // 有样本的零耗时属于有效数据，只有缺失或无效值显示未知。
      duration(value){if(value===null || value===undefined || value==="" || !Number.isFinite(Number(value)) || Number(value)<0)return "—";return Number(value)===0 ? "0 ms" : window.RPFMT.latency(Number(value));},
      setAutoRefresh(enabled){this.autoRefresh=!!enabled;try{window.localStorage.setItem(refreshPreference,String(this.autoRefresh));}catch{}},
      get completed(){return number(this.summary.successes)+number(this.summary.failures);},
      get tokenCoverage(){return this.summary.requests ? Math.min(100,number(this.summary.tokens_reported)/number(this.summary.requests)*100) : 0;},
      get windowLabel(){return this.stats?.window_label || this.window;},
      get windowOptions(){return this.stats?.windows || [{key:"1h",label:"1 小时"},{key:"24h",label:"24 小时"},{key:"7d",label:"7 天"},{key:"30d",label:"30 天"}];},
      get series(){return (this.stats?.series || []).map(p=>({...p,label:localLabel(p.start)}));},
      get chartUnit(){return this.chartMode==="tokens" ? "Token" : this.chartMode==="latency" ? "毫秒" : "次请求";},
      get chartHasData(){return this.chartMode==="tokens" ? number(this.summary.tokens_reported)>0 : this.chartMode==="latency" ? number(this.summary.latency_samples)>0 : number(this.summary.requests)>0;},
      get chartEmpty(){if(!this.stats)return "统计暂不可用，请刷新或重试。";return this.chartMode==="tokens" && number(this.summary.requests)>0 ? "上游未返回用量，暂无 Token 趋势。" : this.chartMode==="latency" && number(this.summary.requests)>0 ? "尚无完成请求的延迟样本。" : "所选时间范围内没有调用记录，开始聊天后这里会显示趋势。";},
      get chartLines(){
        if(this.chartMode==="tokens")return [{key:"total_tokens",label:"总量",cls:"line-tok",color:"#4f6bed",valid:"tokens_reported"},{key:"prompt_tokens",label:"输入",cls:"line-ok",color:"#16876a",valid:"tokens_reported"},{key:"completion_tokens",label:"输出",cls:"line-err",color:"#2992b3",valid:"tokens_reported"}];
        if(this.chartMode==="latency")return [{key:"avg_latency_ms",label:"平均响应耗时",cls:"line-tok",color:"#4f6bed",valid:"latency_samples"}];
        return [{key:"successes",label:"成功",cls:"line-ok",color:"#16876a"},{key:"failures",label:"失败",cls:"line-err",color:"#d45b62"},{key:"in_flight",label:"进行中",cls:"line-pending",color:"#bb821d"}];
      },
      get chart(){
        const points=this.series,lines=this.chartLines;let max=0;
        for(const p of points)for(const l of lines)if(!l.valid || number(p[l.valid]))max=Math.max(max,number(p[l.key]));
        const scale=max||1,step=points.length>1 ? 100/(points.length-1) : 100;
        // 缺少用量或延迟样本时断开线段，避免把未知画成零。
        const paths=lines.map(l=>{let connected=false;let d="";const samples=[];points.forEach((p,i)=>{if(l.valid && !number(p[l.valid])){connected=false;return;}const x=i*step,y=94-number(p[l.key])/scale*88;samples.push({x,y,index:i});d+=(connected?" L":" M")+x.toFixed(2)+","+y.toFixed(2);connected=true;});return {...l,d,samples};});
        return {max,paths,step,points:points.map((p,i)=>({index:i,x:i*step,label:p.label}))};
      },
      get selectedPoint(){return this.chartIndex===null ? null : this.series[this.chartIndex] || null;},
      get chartTimezone(){try{return Intl.DateTimeFormat().resolvedOptions().timeZone;}catch{return "浏览器本地时间";}},
      selectChartMode(mode){this.chartMode=mode;this.chartIndex=null;},
      get distribution(){return states.map(s=>({...s,value:this.counts[s.key]}));},
      get ringStyle(){let pos=0;const total=this.counts.total;const stops=this.distribution.map(s=>{const begin=pos;pos+=total?s.value/total*100:0;return s.color+" "+begin+"% "+pos+"%";});return {background:total?"conic-gradient("+stops.join(",")+")":"#e9edf5"};},
      get channels(){
        const groups=new Map();for(const a of this.accounts){const key=a.provider+"/"+a.region;if(!groups.has(key))groups.set(key,{key,label:this.providerLabel(a.provider)+(a.region==="cn"?" · 国内":a.region==="global"?" · 国际":""),total:0,ready:0,cooldown:0,offline:0,disabled:0});const row=groups.get(key);row.total++;row[accountState(a)]++;}
        return [...groups.values()].sort((a,b)=>b.total-a.total || a.label.localeCompare(b.label));
      },
      providerLabel(id){return providerNames[id] || id || "未分配渠道";},
      share(count,total){return total>0 ? Math.min(100,number(count)/number(total)*100) : 0;},
      get issues(){
        const out=[];
        for(const a of this.accounts){if(!a.enabled)continue;let detail="",priority=1;
          if(!a.has_credential){detail="缺少凭证，补充后才能调用";priority=0;}
          else if(a.ready===false){detail="凭证验证异常，请检查登录态或密钥";priority=0;}
          else if(a.status==="cooldown")detail="暂时冷却，预计 "+window.RPFMT.dur(a.retry_after_seconds)+"后重试";
          else if(!(a.models||[]).length)detail="暂无模型目录，请在账号详情配置或刷新";
          else if(a.last_checkin_status==="failed" || a.last_checkin_status==="error")detail="最近领取失败，请查看账号详情中的记录";
          if(detail)out.push({id:a.id,name:a.name||a.id,detail,priority});
        }
        return out.sort((a,b)=>a.priority-b.priority);
      },
      get serviceTitle(){if(this.error)return "账号状态更新失败";if(!this.ready?.ok)return "服务状态暂不可确认";if(!this.counts.total)return "先添加一个账号，开始使用";if(!this.counts.ready)return "目前没有可调度账号";return "号池已准备好，可开始聊天";},
      get serviceNote(){if(this.error)return "下方可能是上次成功读取的数据，点击刷新重试。";if(!this.ready?.ok)return "其他模块仍可查看，请刷新核对后台状态。";return "可调度表示凭证已配置且未冷却，具体模型权限可在聊天台测试。";},
      get armorLabel(){return this.keyArmor===null ? "状态未知" : this.keyArmor.enabled ? "已开启" : "已关闭";},
      get scheduleLabel(){if(!this.schedule)return "计划未读取";if(!this.schedule.enabled)return "后台已关闭";return (this.schedule.hours||[]).map(h=>String(h).padStart(2,"0")+":00").join("、") || "未配置执行时间";},
      get scheduleTimezone(){if(!this.schedule)return "";if(this.schedule.timezone==="Asia/Shanghai")return "北京时间";return "服务器时间 UTC"+(this.schedule.utc_offset || "（未知）");},
      get checkinCounts(){const eligible=this.accounts.filter(a=>a.enabled && a.checkin?.supported);return {eligible:eligible.length,active:eligible.filter(a=>a.auto_checkin).length,recorded:eligible.filter(a=>["success","already"].includes(a.last_checkin_status)).length};},
      async reload(){
        if(this.refreshing)return;this.refreshing=true;this.loading=!this.lastUpdated;this.error="";
        try{
          const jobs=[['version',()=>RP.version()],['ready',()=>RP.readyz()],['accounts',()=>RP.listAccounts()],['armor',()=>RP.listArmor()],['keyArmor',()=>RP.keyPermissions().then(k=>k.armor)],['schedule',()=>RP.checkinSchedule()],['recent',()=>RP.listRequests({limit:5})]];
          const results=await Promise.allSettled(jobs.map(([,run])=>run()));
          results.forEach((result,i)=>{const key=jobs[i][0];if(result.status==='fulfilled'){this.sectionErrors[key]="";this[key]=key==='accounts'||key==='recent' ? result.value.data||[] : result.value;}else{this.sectionErrors[key]="读取失败";if(key==='accounts')this.error=result.reason?.message || "账号列表读取失败";else if(['ready','keyArmor','schedule'].includes(key))this[key]=null;}});
          if(!this.error)this.lastUpdated=new Date().toISOString();
          await this.loadStats();
        }finally{this.loading=false;this.refreshing=false;}
      },
      async loadStats(requested=this.window){
        if(this.statsLoading)return false;this.statsLoading=true;this.statsError="";
        // 新范围读取成功后再切换选中项，失败时仍展示原范围及其数据。
        try{const data=await RP.stats(requested);this.stats=data;this.window=requested;this.chartIndex=null;return true;}catch(e){this.statsError=e?.message||"统计读取失败";return false;}finally{this.statsLoading=false;}
      },
      async setWindow(key){if(!key || key===this.window || this.statsLoading || this.refreshing || !this.windowOptions.some(option=>option.key===key))return;await this.loadStats(key);},
    });
    return vm;
  }};
  // 只有当前可见的概览页轮询；离开、退出登录或正在读取时不再刷新。
  setInterval(()=>{const app=window.RPAPP,vm=app?.overview;if(app?.authed && app.route==='overview' && vm?.autoRefresh && !document.hidden && !vm.refreshing && !vm.statsLoading)vm.reload().catch(()=>{});},30000);
})();
