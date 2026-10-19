// 完整聊天工作区；会话只保存在当前浏览器，模型调用仍由后端号池执行。
(function(){
  "use strict";
  const historyPrefix="relaypool.chat.v1.";
  const makeID=()=>Date.now().toString(36)+Math.random().toString(36).slice(2,10);
  function storageKey(){return historyPrefix+encodeURIComponent(RP.baseUrl());}
  const tokenLimits=[1024,2048,4096,8192];
  function newConversation(){return {id:makeID(),title:"新对话",model:"",account:"",system:"",draft:"",maxTokens:2048,requestUsage:true,updated:Date.now(),messages:[]};}
  function loadHistory(key=storageKey()){
    try {
      const raw=localStorage.getItem(key);
      if(!raw || raw.length>2*1024*1024)return [];
      const data=JSON.parse(raw);
      if(!Array.isArray(data))return [];
      return data.slice(0,50).filter(c=>c && typeof c.id==="string" && typeof c.title==="string" && Array.isArray(c.messages) && c.messages.every(m=>m && ["user","assistant"].includes(m.role) && typeof m.content==="string" && m.content.length<=100000)).map(c=>({...c,model:typeof c.model==="string"?c.model:"",account:typeof c.account==="string"?c.account:"",system:typeof c.system==="string"?c.system:"",draft:typeof c.draft==="string"?c.draft.slice(0,30000):"",maxTokens:tokenLimits.includes(Number(c.maxTokens))?Number(c.maxTokens):2048,requestUsage:c.requestUsage!==false,messages:c.messages.slice(-200).map(m=>({...m,status:m.status==="streaming"?"stopped":m.status}))}));
    }catch{return [];}
  }
  function activeConversation(history,key){try{return history.find(c=>c.id===localStorage.getItem(key+".active"))||history[0];}catch{return history[0];}}
  function download(name,text){
    const url=URL.createObjectURL(new Blob([text],{type:"text/plain;charset=utf-8"}));
    const link=document.createElement("a");link.href=url;link.download=name;link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
  }
  async function copy(text){
    if(navigator.clipboard && window.isSecureContext){try{await navigator.clipboard.writeText(text);return;}catch{/* 浏览器拒绝剪贴板权限时使用文本复制。 */}}
    const active=document.activeElement,start=active?.selectionStart,end=active?.selectionEnd,direction=active?.selectionDirection;
    const input=document.createElement("textarea");input.value=text;input.style.position="fixed";input.style.opacity="0";
    // 局域网 HTTP 的复制不能抢走草稿焦点，失败时也必须移除临时文本框。
    try{document.body.append(input);input.select();if(!document.execCommand("copy"))throw new Error("浏览器不能复制，请手动选中消息复制");}
    finally{input.remove();if(active?.isConnected){active.focus({preventScroll:true});if(typeof start==="number" && typeof end==="number" && active.setSelectionRange)active.setSelectionRange(start,end,direction);}}
  }
  window.RPChat={createVM(reactive,nextTick,toast){
    let controller=null,saveTimer=null,scrollPending=false,forceScroll=false,resizeObserver=null,observedPane=null,paneHeight=0,layoutPending=false;
    let savedKey=storageKey();
    const history=loadHistory(savedKey);if(!history.length)history.push(newConversation());
    const active=activeConversation(history,savedKey);
    const vm=reactive({
      // 手机按需展开模型和账号选项，把主要空间留给对话；不改变选择或存储。
      modelControlsOpen:false,
      conversations:history,activeID:active.id,models:[],accounts:[],accountsLoaded:false,keyPolicy:null,input:active.draft,busy:false,stopping:false,error:"",notice:"",storageError:"",loading:false,manualModel:false,modelQuery:"",historyQuery:"",historyOpen:false,renaming:false,titleInput:"",pendingDelete:false,followOutput:true,
      armorPreview:null,armorPreviewError:"",armorPreviewBusy:false,armorPreviewSequence:0,
      async inspectArmor(){
        const model=this.current.model.trim(),account=this.current.account,sequence=++this.armorPreviewSequence;
        this.armorPreview=null;this.armorPreviewError="";this.armorPreviewBusy=false;
        if(!model || typeof RP.resolveArmor!=="function")return;
        this.armorPreviewBusy=true;
        try{const result=await RP.resolveArmor(model,account);if(sequence===this.armorPreviewSequence && model===this.current.model.trim() && account===this.current.account)this.armorPreview=result;}
        catch(e){if(sequence===this.armorPreviewSequence)this.armorPreviewError="预设诊断读取失败："+e.message;}
        finally{if(sequence===this.armorPreviewSequence)this.armorPreviewBusy=false;}
      },
      modelChanged(){this.persist();this.inspectArmor();},
      // 只说明提示是否进入网关请求；失败时不能称为上游已执行。
      armorExplanation(message){
        const labels={injected:"预设已加入网关请求",disabled:"当前密钥未启用破甲",request_disabled:"本次请求关闭预设",model_disabled:"这个型号已单独关闭",no_presets:"没有加载预设",unknown_model:"实际型号无法识别，未加入预设",no_matching_preset:"这个系列没有可用预设",selection_forbidden:"不允许请求指定预设",configuration_error:"预设配置不可用"};
        const label=labels[message.armor_status] || (message.armor==="none"?"本次未应用预设":message.armor && message.armor!=="unknown"?"网关报告已应用预设":"没有收到预设诊断");
        return label+(message.status==="failed"?"；请求失败，上游执行未确认":"；不代表模型限制已被突破");
      },
      get current(){return this.conversations.find(c=>c.id===this.activeID)||this.conversations[0];},
      get maxTokens(){return this.current.maxTokens;},
      set maxTokens(value){this.current.maxTokens=Number(value);},
      get requestUsage(){return this.current.requestUsage;},
      set requestUsage(value){this.current.requestUsage=!!value;},
      get shouldRequestUsage(){const provider=this.selectedAccount?.provider||this.models.find(m=>m.id===this.current.model)?.owned_by;return this.requestUsage && ["openai","openai-compatible","deepseek","grok","qwen","glm","kimi","doubao","hunyuan","mimo"].includes(provider);},
      get enabledAccounts(){return this.accounts.filter(a=>a.enabled && this.accountAllowed(a.id));},
      accountAllowed(id){return !!this.keyPolicy && (this.keyPolicy.all_accounts || this.keyPolicy.account_ids.includes(id));},
      modelAllowed(id,channel=""){if(!this.keyPolicy)return false;if(this.keyPolicy.all_models)return true;const canon=v=>String(v).trim().toLowerCase().replace(/[_ ]/g,"-");const slash=id.indexOf("/");if(slash>0){channel=id.slice(0,slash);id=id.slice(slash+1);}return this.keyPolicy.models.some(rule=>{const at=rule.indexOf("/");return at<0 ? canon(rule)===canon(id) : rule.slice(0,at)===channel && canon(rule.slice(at+1))===canon(id);});},
      get selectedAccount(){return this.accounts.find(a=>a.id===this.current.account);},
      get availableModels(){
        const models=this.current.account ? (this.selectedAccount?.models||[]).map(id=>({id})) : this.models;
        const seen=new Set();return models.filter(m=>{if(!m || typeof m.id!=="string" || !m.id || seen.has(m.id) || !this.modelAllowed(m.id,this.selectedAccount?.provider||m.owned_by||""))return false;seen.add(m.id);return true;});
      },
      get filteredModels(){const query=this.modelQuery.trim().toLowerCase();return this.availableModels.filter(m=>!query || m.id.toLowerCase().includes(query));},
      get filteredConversations(){const query=this.historyQuery.trim().toLowerCase();return this.conversations.filter(c=>!query || c.title.toLowerCase().includes(query));},
      get accountName(){return this.current.account ? this.selectedAccount?.name||"原指定账号不可用" : "号池自动调度";},
      get accountProblem(){if(!this.accountsLoaded)return "账号列表尚未读取，请刷新重试";if(this.current.account){const a=this.selectedAccount;if(!this.accountAllowed(this.current.account))return "当前密钥未获准调用原指定账号，请重新选择";if(!a)return "原指定账号已删除，请重新选择账号";if(!a.enabled)return "指定账号已停用，请重新选择或到账号池启用";if(!a.has_credential)return "指定账号未配置凭证，请到账号池补充";}else if(!this.enabledAccounts.some(a=>a.has_credential))return "暂无启用且已配置凭证的账号，请先到账号池添加";return "";},
      get canSend(){return !this.busy && !this.loading && !this.accountProblem && !!this.input.trim() && !!this.current.model.trim();},
      get canRetry(){const m=this.current.messages,n=m.length;return !this.busy && !this.loading && !this.accountProblem && !!this.current.model.trim() && n>=2 && m[n-2].role==="user" && m[n-1].role==="assistant" && ["failed","stopped"].includes(m[n-1].status);},
      get modelListed(){return this.availableModels.some(m=>m.id===this.current.model);},
      get modelVisible(){return this.filteredModels.some(m=>m.id===this.current.model);},
      toggleModelInput(){if(this.busy)return;this.manualModel=!this.manualModel;},
      persist(){
        clearTimeout(saveTimer);saveTimer=null;this.current.draft=this.input;
        try{const value=JSON.stringify(this.conversations);if(value.length>2*1024*1024)throw new Error("会话历史超过 2 MB，请导出后删除旧会话");localStorage.setItem(savedKey,value);localStorage.setItem(savedKey+".active",this.activeID);this.storageError="";}catch(e){this.storageError="历史未保存："+e.message;}
      },
      // 输入时延迟保存，避免每个按键都序列化全部历史；切换和发送立即落盘。
      draftChanged(){this.current.draft=this.input;clearTimeout(saveTimer);saveTimer=setTimeout(()=>this.persist(),500);this.resizeInput();},
      resizeInput(){nextTick(()=>{const el=document.getElementById("chat-input");if(el){el.style.height="auto";el.style.height=Math.min(180,Math.max(72,el.scrollHeight))+"px";}});},
      async reload(){
        if(this.loading || this.busy)return;this.loading=true;this.error="";
        // 不同后端的会话分开保存，切换连接后恢复对应历史。
        if(storageKey()!==savedKey){this.persist();savedKey=storageKey();const restored=loadHistory(savedKey);if(!restored.length)restored.push(newConversation());this.conversations=restored;this.activeID=activeConversation(restored,savedKey).id;this.input=this.current.draft;this.renaming=false;this.pendingDelete=false;this.modelQuery="";this.historyQuery="";this.models=[];this.accounts=[];this.accountsLoaded=false;this.notice="";this.followOutput=true;}
        try{const [models,accounts,key]=await Promise.allSettled([RP.request("GET","/v1/models"),RP.listAccounts(),RP.keyPermissions()]);const errors=[];if(key.status==="fulfilled")this.keyPolicy=key.value.permissions;else{this.keyPolicy=null;errors.push("密钥权限读取失败，请刷新重试");}if(models.status==="fulfilled")this.models=models.value.data||[];else errors.push("模型目录读取失败，未更新的目录仅供参考");if(accounts.status==="fulfilled"){this.accounts=accounts.value.data||[];this.accountsLoaded=true;}else errors.push("账号列表读取失败，请刷新重试");this.error=errors.join("；");if(!this.current.model && this.availableModels.length)this.current.model=this.availableModels[0].id;}finally{this.loading=false;this.inspectArmor();this.resizeInput();this.scroll();}
      },
      select(id){if(this.busy || !this.conversations.some(c=>c.id===id))return;this.persist();this.activeID=id;this.input=this.current.draft;this.error="";this.notice="";this.renaming=false;this.pendingDelete=false;this.modelQuery="";this.persist();this.inspectArmor();this.resizeInput();this.scroll(true);},
      create(){if(this.busy)return;if(this.conversations.length>=50){toast("最多保留 50 个会话，请先导出或删除旧会话",false);return;}this.persist();const c=newConversation();c.model=this.current.model;c.account=this.current.account;c.maxTokens=this.maxTokens;c.requestUsage=this.requestUsage;this.conversations.unshift(c);this.activeID=c.id;this.input="";this.error="";this.notice="";this.historyQuery="";this.renaming=false;this.pendingDelete=false;this.persist();this.inspectArmor();this.resizeInput();this.scroll(true);},
      requestRemove(){if(!this.busy)this.pendingDelete=true;},
      remove(){if(this.busy || !this.pendingDelete)return;this.conversations=this.conversations.filter(c=>c.id!==this.activeID);if(!this.conversations.length)this.conversations.push(newConversation());this.activeID=this.conversations[0].id;this.input=this.current.draft;this.pendingDelete=false;this.renaming=false;this.error="";this.notice="";this.persist();this.inspectArmor();this.resizeInput();this.scroll(true);},
      beginRename(){if(this.busy)return;this.titleInput=this.current.title;this.renaming=true;nextTick(()=>document.getElementById("chat-title-input")?.focus());},
      rename(){const title=this.titleInput.trim();if(!title)return;this.current.title=title.slice(0,60);this.renaming=false;this.persist();},
      accountChanged(){this.manualModel=false;this.modelQuery="";this.current.model=this.availableModels[0]?.id||"";this.modelChanged();},
      // 路由先读取数据再挂载，挂载后才能恢复长对话的最新位置。
      mounted(){this.unmounted();observedPane=document.getElementById("chat-messages");paneHeight=observedPane?.clientHeight||0;this.resizeInput();this.scroll(true);if(typeof ResizeObserver!=="undefined" && observedPane){resizeObserver=new ResizeObserver(()=>{if(observedPane && observedPane.clientHeight!==paneHeight){paneHeight=observedPane.clientHeight;layoutPending=true;this.scroll();}});resizeObserver.observe(observedPane);}},
      unmounted(){resizeObserver?.disconnect();resizeObserver=null;observedPane=null;paneHeight=0;layoutPending=false;},
      onScroll(){const el=document.getElementById("chat-messages");if(!el)return;if(el===observedPane && el.clientHeight!==paneHeight){paneHeight=el.clientHeight;layoutPending=true;this.scroll();return;}if(!layoutPending)this.followOutput=el.scrollHeight-el.clientHeight-el.scrollTop<=48;},
      // 合并同一帧的滚动；用户回看旧消息时不抢回底部。
      scroll(force=false){if(force){this.followOutput=true;forceScroll=true;}if(scrollPending)return;scrollPending=true;nextTick(()=>requestAnimationFrame(()=>{scrollPending=false;const el=document.getElementById("chat-messages");if(el && (forceScroll || this.followOutput))el.scrollTop=el.scrollHeight;forceScroll=false;layoutPending=false;}));},
      stop(){if(controller){this.stopping=true;controller.abort();}},
      elapsed(message){const ms=message.elapsed_ms;if(!Number.isFinite(ms))return "";return ms<1000 ? Math.round(ms)+" 毫秒" : (ms/1000).toFixed(1)+" 秒";},
      usageLabel(message){const u=message.usage;if(!u)return "用量未上报";const counts=[["prompt_tokens","输入"],["completion_tokens","输出"],["total_tokens","总量"]].filter(([key])=>Number.isInteger(u[key]) && u[key]>=0).map(([key,label])=>label+" "+u[key]);return counts.length ? counts.join(" · ")+" Token" : "用量未上报";},
      async copy(message){try{await copy(message.content);toast("消息已复制");}catch(e){toast(e.message,false);}},
      export(){const c=this.current;download("聊天记录-"+c.id+".md", "# "+c.title+"\n\n模型："+c.model+"\n\n"+c.messages.map(m=>"## "+(m.role==="user"?"你":"助手")+"\n\n"+m.content+(m.status!=="complete"?"\n\n["+(m.status==="stopped"?"已停止":"回复不完整")+"]":"")+(m.previous?"\n\n### 上次未完成的回复\n\n"+(m.previous.reasoning?"思考过程：\n\n"+m.previous.reasoning+"\n\n":"")+m.previous.content:"")).join("\n\n"));},
      retry(){if(this.canRetry)return this.send(true);},
      async send(retry=false){
        if(this.busy || this.loading)return;
        if(retry && !this.canRetry)return;
        const c=this.current,text=retry ? c.messages[c.messages.length-2].content : this.input.trim();
        if(!text)return;
        if(this.accountProblem){this.error=this.accountProblem;return;}
        if(!c.model.trim()){this.error="请先选择或输入模型名称";return;}
        if(text.length>30000){this.error="单条消息不能超过 30000 字符";return;}
        if(!retry && c.messages.length+2>200){this.error="本会话已达 200 条消息，请新建会话";return;}
        if(!tokenLimits.includes(Number(this.maxTokens))){this.error="请选择有效的输出上限";return;}
        // 只有完整问答进入后续上下文，取消或失败的回复保留展示但不重发。
        const messages=[];
        if(c.system.trim())messages.push({role:"system",content:c.system.trim()});
        const contextEnd=c.messages.length-(retry?2:0);
        for(let i=0;i+1<contextEnd;i+=2){const u=c.messages[i],a=c.messages[i+1];if(u.role==="user" && a.role==="assistant" && a.status==="complete")messages.push({role:"user",content:u.content},{role:"assistant",content:a.content});}
        messages.push({role:"user",content:text});
        if(JSON.stringify(messages).length>600000){this.error="对话上下文过大，请新建会话";return;}
        const started=Date.now();
        const answer=reactive({id:makeID(),role:"assistant",content:"",reasoning:"",status:"streaming",time:started,model:c.model,account:"",finish:"",armor:"",armor_status:"",armor_sha256:"",armor_bytes:null,request_id:"",error:"",elapsed_ms:null,usage:null});
        // 只保留最近一份有内容的未完成回复，不叠加进上下文，也不混用两次用量。
        if(retry){const last=c.messages.at(-1),prior=(last.content || last.reasoning)?last:last.previous;if(prior)answer.previous={content:prior.content||"",reasoning:prior.reasoning||"",model:prior.model,account:prior.account,armor:prior.armor,usage:prior.usage,elapsed_ms:prior.elapsed_ms};}
        if(retry)c.messages.splice(-1,1,answer);else{c.messages.push({id:makeID(),role:"user",content:text,status:"complete",time:started},answer);this.input="";}
        if(c.title==="新对话")c.title=text.slice(0,30);
        c.updated=started;this.error="";this.notice="";this.busy=true;this.stopping=false;controller=new AbortController();this.persist();this.resizeInput();this.scroll(true);
        const path=c.account?"/admin/accounts/"+encodeURIComponent(c.account)+"/chat/completions":"/v1/chat/completions";
        try {
          const body={model:c.model.trim(),messages,stream:true,max_tokens:Number(this.maxTokens)};
          if(this.shouldRequestUsage)body.stream_options={include_usage:true};
          await RP.stream(path,body,event=>{
            const usage=event.usage;
            if(usage && typeof usage==="object"){const counts={};for(const key of ["prompt_tokens","completion_tokens","total_tokens"])if(Number.isInteger(usage[key]) && usage[key]>=0)counts[key]=usage[key];if(Object.keys(counts).length)answer.usage={...answer.usage,...counts};}
            const choice=event.choices && event.choices[0];if(!choice)return;
            const delta=choice.delta||{};
            if(typeof delta.content==="string")answer.content+=delta.content;
            if(typeof delta.reasoning_content==="string")answer.reasoning+=delta.reasoning_content;
            if(event.model)answer.model=event.model;
            if(choice.finish_reason)answer.finish=choice.finish_reason;
            if(answer.content.length+answer.reasoning.length>100000){controller.abort();throw new Error("回复超过 100000 字符，已停止接收");}
            this.scroll();
          },controller.signal,headers=>{
            answer.account=headers.get("X-RelayPool-Account")||"";
            // 记录本次响应的实际预设，不用页面开关推测这条消息是否应用。
            answer.armor=headers.get("X-RelayPool-Armor-Applied")||"unknown";
            answer.armor_status=headers.get("X-RelayPool-Armor-Status")||"";
            answer.armor_sha256=headers.get("X-RelayPool-Armor-SHA256")||"";
            answer.request_id=headers.get("X-Request-ID")||"";
            const bytes=headers.get("X-RelayPool-Armor-Bytes");answer.armor_bytes=bytes!==null && /^\d+$/.test(bytes)?Number(bytes):null;
          });
          if(!answer.content.trim())throw new Error("上游没有返回可显示的文本");
          answer.status="complete";
          delete answer.previous;
          if(answer.finish==="length")this.notice="回复达到输出上限，可在对话设置中调高上限后发送‘继续’";
        }catch(e){answer.status=e.name==="AbortError"?"stopped":"failed";if(answer.status==="stopped")this.notice="已停止生成，已收到的内容仍保留";else{answer.error=e.message||"回复失败，请重试";this.error=answer.error;}}
        finally{answer.elapsed_ms=Date.now()-started;this.busy=false;this.stopping=false;controller=null;this.persist();this.scroll();}
      }
    });
    return vm;
  }};
  // 刷新或关闭页面前立即保存，补上输入延迟保存尚未触发的草稿。
  window.addEventListener("pagehide",()=>window.RPAPP?.chat?.persist());
})();
