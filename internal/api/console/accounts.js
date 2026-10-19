// 账号页的筛选与凭证识别只处理用户主动提供的数据，不读取本机登录文件。
(function(){
  "use strict";
  const credentialKind=input=>{
    const key=input?.api_key || input?.OPENAI_API_KEY;
    return typeof key==="string" && key.trim()?{auth_type:"api_key",key}: {auth_type:"oauth",key:""};
  };
  const state=a=>!a.enabled?"disabled":!a.has_credential?"missing":a.status==="cooldown"?"cooldown":a.status==="ready" && a.ready!==false?"ready":"unverified";
  window.RPAccounts={
    state,credentialKind,
    region(input,family){
      const auth=input.auth || input;const region=auth.region || input.region;
      if(region==="global" || region==="intl")return "global";
      if(region==="cn")return "cn";
      if(family==="trae-intl")return "global";
      let domain=String(auth.domain || input.domain || "").trim().toLowerCase();
      try{domain=new URL(domain.includes("://")?domain:"https://"+domain).hostname;}catch{return "";}
      const match=base=>domain===base || domain.endsWith("."+base);
      if(family==="workbuddy"){
        if(["www.workbuddy.ai","www.codebuddy.ai"].some(match))return "global";
        if(["codebuddy.cn","copilot.tencent.com"].some(match))return "cn";
      }
      if(family==="qoder"){if(["qoder.com","qoder.sh"].some(match))return "global";if(match("qoder.cn"))return "cn";}
      if(family==="trae" && match("trae.cn"))return "cn";
      return "";
    },
    filter(list,options){
      const needle=String(options.query || "").trim().toLowerCase();
      let rows=list.filter(a=>(!options.provider || a.provider===options.provider) && (!options.region || a.region===options.region) && (!options.state || state(a)===options.state) && (!needle || [a.name,a.id,a.provider,...(a.models || [])].join(" ").toLowerCase().includes(needle)));
      if(options.sort==="name")rows=rows.slice().sort((a,b)=>String(a.name || a.id).localeCompare(String(b.name || b.id),"zh-CN"));
      return rows;
    },
    summary(list){return {total:list.length,enabled:list.filter(a=>a.enabled).length,ready:list.filter(a=>state(a)==="ready").length,missing:list.filter(a=>!a.has_credential).length};}
  };
})();
