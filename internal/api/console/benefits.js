(function () {
  "use strict";
  const platforms = {
    workbuddy: {name:"WorkBuddy", url:"https://www.workbuddy.cn/"},
    qoder: {name:"Qoder", url:"https://qoder.com/"},
    trae: {name:"TRAE 国内版", url:"https://www.trae.cn/"},
    codearts: {name:"华为云码道", url:"https://www.huaweicloud.com/product/codearts/codearts-code.html"},
    officeace: {name:"华为果办", url:"https://www.huaweicloud.com/"}
  };
  const statusNames={active:"活动中",claimable:"开放领取",ending:"即将结束",standing:"常驻福利",open:"开放申请",upcoming:"尚未开始",ended:"已结束",stale:"待重新核实",withdrawn:"已撤回"};
  const requirementNames={none:"目录未列额外门槛",phone:"海外手机号",card:"海外支付方式",net:"海外网络",student:"学生认证",biz:"企业认证",invite:"邀请码",realname:"实名认证",pay:"需付费或充值",targeted:"定向资格",newUser:"新用户",member:"会员身份"};
  const typeNames={new:"新人福利",trial:"试用额度",free:"免费使用",perk:"会员权益",topup:"充值赠送",gift:"赠送额度",standing:"周期福利",apply:"申请制"};
  function safeURL(value) {
    try { const u=new URL(value);return u.protocol==="https:"&&!u.username&&!u.password?u.href:""; } catch { return ""; }
  }
  function period(value,cap={}) {
    const n=Date.parse(value);if(!Number.isFinite(n))return "";
    // Match the server's per-platform day boundary, including Qoder's 10:00 reset.
    const offset=cap.timezone==="UTC+8"?8*3600000:0;
    return new Date(n+offset-(Number(cap.day_start_hour)||0)*3600000).toISOString().slice(0,10);
  }
  function completed(a,now=new Date().toISOString()) {
    return ["success","already"].includes(a.last_checkin_status)&&period(a.last_checkin_at,a.checkin)===period(now,a.checkin);
  }
  function normalizeOffer(o) {
    const text=v=>typeof v==="string"?v:"";
    return {...o,id:text(o.id),title:text(o.title)||"未命名福利",platform:text(o.platform)||"未标注平台",valueText:text(o.valueText)||"额度请查看官方说明",link:safeURL(o.link),
      requirements:Array.isArray(o.requirements)?o.requirements.filter(x=>typeof x==="string").map(x=>requirementNames[x]||x):[],
      statusLabel:statusNames[o.status]||"状态待确认",typeLabel:typeNames[o.type]||"平台福利"};
  }
  function createVM(reactive) {
    let loadRevision=0,feedRevision=0,detailRevision=0,lifecycle=0;
    const identity=()=>RP.baseUrl()+"\n"+RP.key.value;
    const vm=reactive({
      tab:"discover",accounts:[],accountsError:"",accountsLoading:false,accountsLoaded:false,
      offers:[],feed:null,feedError:"",feedLoading:false,feedLoaded:false,query:"",modality:"",status:"all",offset:0,previousOffsets:[],
      busy:{},results:{},now:new Date().toISOString(),
      detail:null,detailID:"",detailLoading:false,detailError:"",detailStale:false,
      get detailView(){return this.detail||{requirements:[],steps:[],eligibilityNotes:[]};},
      get claimAccounts(){return this.accounts.filter(a=>a.checkin?.supported);},
      get pendingAccounts(){return this.claimAccounts.filter(a=>a.enabled&&a.has_credential&&!completed(a,this.now));},
      get completedCount(){return this.claimAccounts.filter(a=>completed(a,this.now)).length;},
      get total(){return Number(this.feed?.data?.total)||0;},
      get nextOffset(){return this.feed?.data?.nextOffset??null;},
      get feedWarning(){
        if(this.feed?.stale)return "来源连接失败，正在显示上次缓存。请核对官方页面后领取。";
        if(this.feed?.data?.degraded||this.feed?.data?.coverageDegraded)return "来源目前提供快照或不完整数据，请留意核验时间。";
        return "目录信息不代表你的账号已符合资格；待重新核实的福利请先查看官方规则。";
      },
      platform(a){return platforms[a.provider]?.name||a.provider;},
      isCompleted(a){return completed(a,this.now);},
      canClaim(a){return this.accountsLoaded&&!this.accountsError&&!this.accountsLoading&&a.enabled&&a.has_credential&&a.checkin?.supported&&!this.busy[a.id]&&!this.isCompleted(a);},
      accountState(a){return !a.enabled?"账号已停用":!a.has_credential?"需要登录授权":this.isCompleted(a)?"本周期已领取":"可尝试领取";},
      resultLabel(a){const r=this.results[a.id];return r?({success:"领取成功",already:"已经领过",skipped:"未确认领取",error:"领取失败"}[r.status]||"待确认"):"";},
      result(a){return this.results[a.id]||{status:"",message:""};},
      async reload(){
        const rev=loadRevision+1;
        await this.loadAccounts();
        if(rev!==loadRevision)return;
        if(!this.feedLoaded&&!this.feedLoading)this.loadOffers();
      },
      async loadAccounts(){
        const rev=++loadRevision,who=identity();this.accountsLoading=true;
        try{const r=await RP.listAccounts();if(rev!==loadRevision||who!==identity())return;
          this.accounts=Array.isArray(r.data)?r.data:[];this.accountsError="";this.accountsLoaded=true;this.now=new Date().toISOString();
        }catch(e){if(rev===loadRevision&&who===identity())this.accountsError="账号信息读取失败："+e.message;}
        finally{if(rev===loadRevision&&who===identity())this.accountsLoading=false;}
      },
      async loadOffers(offset=0,history=[]){
        const rev=++feedRevision,who=identity();this.feedLoading=true;this.feedError="";
        const query={q:this.query.trim(),modality:this.modality,status:this.status,offset};
        try{const r=await RP.listBenefits(query);if(rev!==feedRevision||who!==identity())return;
          if(!Array.isArray(r.data?.offers))throw Error("目录返回了无法识别的数据");
          this.offers=r.data.offers.filter(o=>o&&typeof o.id==="string").map(normalizeOffer);this.feed=r;this.feedLoaded=true;this.offset=offset;this.previousOffsets=history;
        }catch(e){if(rev===feedRevision&&who===identity())this.feedError=e.message;}
        finally{if(rev===feedRevision&&who===identity())this.feedLoading=false;}
      },
      next(){if(!this.feedLoading&&this.nextOffset!==null)this.loadOffers(this.nextOffset,[...this.previousOffsets,this.offset]);},
      previous(){if(!this.feedLoading&&this.previousOffsets.length){const p=[...this.previousOffsets],offset=p.pop();this.loadOffers(offset,p);}},
      resetFilters(){this.query="";this.modality="";this.status="all";this.loadOffers();},
      async showDetail(o){
        const rev=++detailRevision,who=identity();this.detailID=o.id;this.detail=null;this.detailError="";this.detailLoading=true;this.detailStale=false;
        if(window.requestAnimationFrame)window.requestAnimationFrame(()=>{if(rev===detailRevision)document.querySelector('.benefits-detail')?.scrollIntoView({behavior:'smooth',block:'start'});});
        try{const r=await RP.benefitDetail(o.id);if(rev!==detailRevision||who!==identity())return;
          if(!r.data?.offer||r.data.offer.id!==o.id)throw Error("福利详情不匹配，请重试");
          const raw=r.data.offer;
          this.detail={...normalizeOffer(raw),steps:Array.isArray(raw.steps)?raw.steps.filter(x=>typeof x==="string"):[],
            officialURL:safeURL(raw.evidence?.url),eligibilityNotes:Array.isArray(raw.eligibility?.reasons)?raw.eligibility.reasons.filter(x=>typeof x==="string"):[]};
          this.detailStale=!!(r.stale||r.data.degraded);
        }catch(e){if(rev===detailRevision&&who===identity())this.detailError=e.message;}
        finally{if(rev===detailRevision&&who===identity())this.detailLoading=false;}
      },
      closeDetail(){detailRevision++;this.detailID="";this.detail=null;this.detailLoading=false;this.detailError="";},
      async claim(a){
        // Only local account capabilities enable this path. Feed claimMethod is
        // informational and can never trigger a POST or create an account.
        if(!this.canClaim(a))return;
        const who=identity(),life=lifecycle;this.busy[a.id]=true;delete this.results[a.id];
        try{const r=await RP.checkinAccount(a.id);if(who!==identity()||life!==lifecycle)return;
          this.results[a.id]={status:r.status,message:r.message||"上游未说明结果，请查看账号详情。"};
          await this.loadAccounts();
        }catch(e){if(who===identity()&&life===lifecycle)this.results[a.id]={status:"error",message:e.message};}
        finally{if(who===identity())this.busy[a.id]=false;}
      },
      dispose(){lifecycle++;loadRevision++;feedRevision++;detailRevision++;this.accountsLoading=false;this.feedLoading=false;this.detailLoading=false;this.detailID="";this.detail=null;}
    });
    return vm;
  }
  window.RPBenefits={createVM,safeURL,period,completed,normalizeOffer};
})();
