// relaypool 控制台 — 应用入口
//
// petite-vue 0.4.x 提供声明式绑定（v-if / v-for / @click / v-model 都支持），
// 但没有组件系统，所以页面用「每页一个视图模型工厂函数 + <template v-if>」组织。
//
// 路由是手写的 hash 路由（#/overview / #/accounts ...）。

(function () {
  "use strict";

  // 语言初始化放在同源脚本中，配合内容安全策略禁止内联脚本。
  dayjs.locale("zh-cn");dayjs.extend(dayjs_plugin_relativeTime);

  const { createApp, reactive, nextTick } = window.PetiteVue;

  // ---------- 工具 ----------

  const fmt = {
	accountStatus(account) {
	  if(!account.enabled)return "已停用";
	  if(account.status==="cooldown")return "冷却中";
	  if(account.ready===false)return "凭证异常";
	  if(account.ready===true)return "就绪";
	  if(account.has_credential)return "待验证";
	  return "未配置";
	},
    time(s) {
      if (!s) return "—";
      const d = window.dayjs ? dayjs(s) : new Date(s);
      if (window.dayjs) return d.format("MM-DD HH:mm:ss");
      return d.toLocaleString();
    },
    timeFull(s) {
      if (!s) return "—";
      return window.dayjs ? dayjs(s).format("YYYY-MM-DD HH:mm:ss") : new Date(s).toLocaleString();
    },
    ago(s) {
      if (!s) return "—";
      return window.dayjs ? dayjs(s).fromNow() : new Date(s).toLocaleString();
    },
    ms(v) {
      if (v === null || v === undefined) return "—";
      if (v < 1000) return v + " ms";
      return (v / 1000).toFixed(2) + " s";
    },
    tokens(v) {
      // token 字段可能缺失：显示 — 而不是 0（未知 ≠ 零消耗）
      if (v === null || v === undefined) return "—";
      return String(v);
    },
    bytes(v) {
      if (v === null || v === undefined) return "—";
      if (v < 1024) return v + " B";
      if (v < 1024 * 1024) return (v / 1024).toFixed(1) + " KB";
      return (v / 1024 / 1024).toFixed(1) + " MB";
    },
    dur(sec) {
      if (sec === null || sec === undefined) return "—";
      if (sec < 60) return Math.ceil(sec) + " 秒";
      if (sec < 3600) return Math.floor(sec / 60) + " 分 " + Math.ceil(sec % 60) + " 秒";
      return Math.floor(sec / 3600) + " 时 " + Math.floor((sec % 3600) / 60) + " 分";
    },
    // compact renders a large count the way an operator scans it: 1.3k / 396M.
    compact(v) {
      if (v === null || v === undefined) return "—";
      const n = Number(v);
      if (!isFinite(n)) return "—";
      const abs = Math.abs(n);
      if (abs >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, "") + "B";
      if (abs >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
      if (abs >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, "") + "k";
      return String(Math.round(n));
    },
    // pct renders a 0-100 number with one decimal, dropping a trailing .0.
    pct(v) {
      if (v === null || v === undefined) return "—";
      const n = Number(v);
      if (!isFinite(n)) return "—";
      return n.toFixed(1).replace(/\.0$/, "") + "%";
    },
    // latency renders a millisecond duration at a readable scale.
    latency(ms) {
      if (ms === null || ms === undefined || !isFinite(Number(ms))) return "—";
      const n = Number(ms);
      if (n <= 0) return "—";
      if (n < 1000) return Math.round(n) + "ms";
      return (n / 1000).toFixed(1).replace(/\.0$/, "") + "s";
    },
  };
  window.RPFMT = fmt;

  function esc(s) {
    return String(s === null || s === undefined ? "" : s).replace(/[&<>"']/g, (c) => {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  window.RPESC = esc;

  // 访问模式徽标
  function accessBadge(mode) {
    return { cls: "b-ok", label: "本机模式" };
  }
  window.RPACCESS = accessBadge;

  // ---------- 应用状态 ----------

  const app = reactive({
    // 路由
    route: "overview",
    routeParam: null,
    get pageTitle() { return ({overview:'概览',chat:'聊天',accounts:'账号池',account:'账号详情',armor:'破甲预设',logs:'请求日志',keys:'API 密钥',settings:'连接设置'})[this.route] || '工作台'; },

    // 连接状态
    authed: false,
    conn: {
      health: null,       // /healthz 的返回
      access: null,       // /admin/access 的返回
      accessBadge: null,
      connError: "",
      checking: false,
    },

    // 全局弹层
    modal: {
      open: false,
      title: "",
      html: "",
      wide: false,
      footer: null, // {confirmText, danger, onConfirm}
    },
    toast: { open: false, text: "", ok: true },

    // 各页面的视图模型（onPageEnter 时填充）
    overview: null,
    chat: null,
    accounts: null,
    accountDetail: null,
    armor: null,
    keys: null,
    logs: null,
    settings: null,
  });

  // ---------- 全局动作 ----------

  window.RPAPP = app;

  function go(route, param) {
    location.hash = param ? "#/" + route + "/" + param : "#/" + route;
  }
  app.go = go;

  function toast(text, ok) {
    app.toast.open = true;
    app.toast.text = text;
    app.toast.ok = ok !== false;
    clearTimeout(toast._t);
    toast._t = setTimeout(() => (app.toast.open = false), 2600);
  }
  app.toastMsg = toast;

  // 错误统一转成用户可读的 toast + console
  function reportErr(e, prefix) {
    const msg = (prefix ? prefix + "：" : "") + (e && e.message ? e.message : String(e));
    toast(msg, false);
    console.warn("[console]", e);
  }
  app.reportErr = reportErr;

  function closeModal() {
    app.modal.open = false;
    app.modal.html = "";
    app.modal.wide = false;
    app.modal.footer = null;
  }
  app.closeModal = closeModal;

  // ---------- 连接与鉴权 ----------

  // 退出、换连接或更新一轮检查后，旧结果不能恢复或覆盖当前身份。
  let connectionRevision = 0;
  function invalidateConnectionChecks() { connectionRevision++; app.conn.checking = false; }
  async function checkConnection() {
    const c = app.conn;
    const revision=++connectionRevision,base=RP.baseUrl(),credential=RP.key.value;
    const current=()=>revision===connectionRevision&&base===RP.baseUrl()&&credential===RP.key.value;
    c.checking = true;
    c.connError = "";
    try {
      // The health probe needs no credential, so it also answers "is the
      // backend even reachable" when no key is configured yet.
      const health = await RP.healthz();
      if(!current())return;
      c.health = health;
    } catch (e) {
      if(!current())return;
      app.authed = false;
      c.access = null;
      c.accessBadge = null;
      c.health = null;
      c.connError = "后端不可达（" + RP.baseUrl() + "）";
      c.checking = false;
      return;
    }
    // Validating the key is only meaningful if one is configured. Probing
    // without one would just produce a 401 the browser reports as a console
    // error, which looks like a defect in a page that has nothing to hide.
    if (!RP.key.value) {
      app.authed = false;
      c.access = null;
      c.accessBadge = null;
      c.connError = "";
      c.checking = false;
      return;
    }
    try {
      const access = await RP.access();
      if(!current())return;
      c.access = access;
      c.accessBadge = accessBadge(c.access.mode);
      app.authed = true;
    } catch (e) {
      if(!current())return;
      app.authed = false;
      c.access = null;
      c.accessBadge = null;
      if (e.status === 401) {
        c.connError = "API Key 无效或缺失";
      } else {
        c.connError = e.message;
      }
    } finally {
      if(current())c.checking = false;
    }
  }
  app.checkConnection = checkConnection;

  app.logout = function () {
    invalidateConnectionChecks();
    navGeneration++;
    // 退出时释放聊天高度观察器；视图移除可能发生在会话对象清空之后。
    if(app.chat){app.chat.stop();app.chat.unmounted();}
    if(app.accounts){app.accounts.stopTests();app.accounts.login?.stopPolling();}
    RP.key.value = "";
    if(app.gate)app.gate.keyInput="";
    RP.saveSettings();
    app.authed = false;
    app.conn.connError = "";
    app.conn.access = null;
    // Clearing the key drops back to the login gate; the view models are reset
    // so a later login with a different key cannot show the previous session's
    // cached data.
    app.overview = null;
    app.chat = null;
    app.accounts = null;
    app.accountDetail = null;
    app.armor = null;
    app.keys = null;
    app.logs = null;
    app.settings = null;
    toast("已清除本机保存的 Key");
  };

  // ---------- 路由 ----------

  const routes = {
    overview: "overview",
    chat: "chat",
    accounts: "accounts",
    account: "accountDetail",
    armor: "armor",
    keys: "keys",
    logs: "logs",
    settings: "settings",
  };

  function parseHash() {
    const h = location.hash.replace(/^#\/?/, "");
    const parts = h.split("/").filter(Boolean);
    const name = routes[parts[0]] ? parts[0] : "overview";
    return { name, param: parts[1] ? decodeURIComponent(parts[1]) : null };
  }

  // ---------- 路由 ----------
  //
  // ONE handler for every navigation, registered exactly once.
  //
  // WHY THIS MATTERS (a real freeze that was fixed here): the page previously
  // registered TWO hashchange listeners, so every navigation ran the route logic
  // twice. Two passes meant two concurrent loads, and whichever finished last
  // won -- often the older one. The user saw a click that appeared to do nothing
  // and then showed the wrong page.
  //
  // The generation counter below makes a superseded navigation unable to apply:
  // each run captures its own generation and aborts if the route has moved on.
  //
  // ORDERING RULE, which is what keeps petite-vue from throwing: `app.route` is
  // assigned only AFTER the target view model has its data. petite-vue patches
  // the DOM as soon as the route changes, and if a `v-for` list is still empty
  // (or is replaced in the same tick as the guard flips) it can be left holding
  // a null anchor and throw "Cannot read properties of null (reading
  // 'insertBefore')". That throw aborts the patch midway, which is exactly the
  // half-rendered, unresponsive state this file used to exhibit.
  let navGeneration = 0;

  async function navigate() {
    if (!app.authed) return;

    const target = parseHash();
    const generation = ++navGeneration;
    const vmKey = routes[target.name];
    if (!vmKey) return;

    // Construct the view model on first visit; this is the only place they are
    // created, so a route cannot be reachable without a view model.
    ensureVM(target.name);
    const vm = app[vmKey];
    if (!vm) return;

    // The account detail page is keyed by a path parameter, so switching from
    // one account to another must re-run its init.
    if (vmKey === "accountDetail") {
      const alreadyLoaded = vm.acc && vm.acc.id === target.param;
      if (!alreadyLoaded) {
        try {
          await vm.init(target.param);
        } catch (e) {
          reportErr(e, "加载账号失败");
        }
      }
      if (generation !== navGeneration) return; // superseded
      // Commit the route only now that the page has data to render.
      app.route = target.name;
      app.routeParam = target.param;
      return;
    }

    if (vm.reload) {
      try {
        await vm.reload();
      } catch (e) {
        reportErr(e, "加载失败");
      }
    }
    if (generation !== navGeneration) return; // superseded
    app.route = target.name;
    app.routeParam = target.param;
  }
  app.navigate = navigate;

  window.addEventListener("hashchange", () => {
    navigate();
  });

  // ---------- 各页面视图模型 ----------

  // ===== 概览 =====
  // 概览独立管理展示状态，保持原路由和管理接口不变。
  function newOverviewVM() { return RPOverview.createVM(reactive); }

  // ===== 账号列表 =====
  function newAccountsVM() {
    // A fresh form object. Kept as a factory so "cancel" and the first open
    // both start from the same defaults; the form previously started as {}
    // and only became complete after a cancel, so the first submit threw.
    function blankForm() {
      return {
        name: "",
        provider: "workbuddy",
        region: "cn",
        weight: 50,
        max_inflight: 0,
        priority: 50,
        quota_scope: "account",
        quota_group: "",
        api_base_url: "",
        api_key: "",
        api_models: "",
		endpoint_url: "",
		workspace_id: "",
        auto_checkin: false,
        drop_system_prompt: true,
        enabled: true,
        credential: "",
        auth_type: "oauth",
        project_id: "",
        client_id: "",
        client_secret: "",
		trae_variant: "solo",
		trae_scene: "solo_work_lite",
      };
    }

    return reactive({
      loading: true,
      error: "",
      list: [],
      createOpen: false,
      creating: false,
      form: blankForm(),
      providers: [],
      query: "",
      providerFilter: "",
      regionFilter: "",
      stateFilter: "",
      sortMode: "created",
      get visibleList(){return RPAccounts.filter(this.list,{query:this.query,provider:this.providerFilter,region:this.regionFilter,state:this.stateFilter,sort:this.sortMode});},
      get summary(){return RPAccounts.summary(this.list);},
      get availableProviders(){return this.providers.filter(p=>this.list.some(a=>a.provider===p.id));},
      clearFilters(){this.query="";this.providerFilter="";this.regionFilter="";this.stateFilter="";this.sortMode="created";},
      quotaBusy: false,
      quotaPending: false,
      quotaPendingForce: false,
      quotaBase: "",
      quotaLabel(a){return RPQuota.label(a.quota || (!a.quota_supported ? {state:"unsupported"} : null));},
      async refreshQuotas(refresh=false){
        // 测试或保存期间已有查询在途时，合并下一次刷新，避免主动刷新被丢弃。
        if(this.quotaBusy){this.quotaPending=true;this.quotaPendingForce ||= refresh;return;}this.quotaBusy=true;
        const base=RP.baseUrl(),queue=this.list.filter(a=>a.quota_supported && a.has_credential).map(a=>a.id);
        this.quotaBase=base;
        for(const a of this.list)if(a.quota_supported && !a.has_credential)a.quota={state:"unknown",message:"尚未配置凭证"};
        const worker=async()=>{while(queue.length){const id=queue.shift();if(RP.baseUrl()!==base)break;let quota;try{quota=await RP.getAccountQuota(id,refresh);}catch{quota={state:"error",message:"额度读取失败，请刷新重试"};}if(RP.baseUrl()===base){const a=this.list.find(a=>a.id===id);if(a)a.quota=quota;}}};
        try{await Promise.all([worker(),worker()]);}finally{this.quotaBusy=false;if(this.quotaPending){const force=this.quotaPendingForce;this.quotaPending=false;this.quotaPendingForce=false;this.refreshQuotas(force);}}
      },
      checkinSaving: {},
      checkinSchedule: null,
      scheduleError: "",
      get scheduleText() {
        const schedule=this.checkinSchedule;
        if(!schedule)return this.scheduleError || "正在读取自动领取计划…";
        if(!schedule.enabled)return "后台自动领取已关闭，账号开关暂不执行；请在部署配置中启用后台领取。";
        const hours=(schedule.hours || []).map(h=>String(h).padStart(2,"0")+":00").join("、");
        return hours ? "自动领取：每天 "+hours+"（"+schedule.timezone+"，UTC"+schedule.utc_offset+"）。成功后本周期不重复；仅执行已启用且打开账号开关的渠道。" : "后台没有设置领取时刻，请在部署配置中设置。";
      },
      testBusy: false,
      testController: null,
      testResults: [],
      testTotal: 0,
      testModel: "",
      testError: "",
      stopTests(){if(this.testController)this.testController.abort();},
      async testAccounts(id,filtered=false){
        if(this.testBusy)return;
        const ids=id?[id]:filtered?this.visibleList.map(a=>a.id):[];
        if(filtered && !ids.length)return;
        this.testBusy=true;this.testError="";this.testResults=[];this.testTotal=0;this.testController=new AbortController();
        try {
          await RP.stream("/admin/accounts/test",{ids,model:this.testModel.trim()},event=>{
            if(event.type==="start")this.testTotal=event.total;
            if(event.type==="result")this.testResults.push(event.result);
          },this.testController.signal);
        }catch(e){this.testError=e.name==="AbortError"?"已停止，已完成的结果保留":e.message;}
        finally{this.testBusy=false;this.testController=null;await this.reload({refreshQuota:true});}
      },
      get selectedProvider() {return this.providers.find(p=>p.id===this.form.provider) || {auth_types:["oauth"],note:""};},
      get usesAPIKey() {return this.form.provider!=="workbuddy" && this.form.auth_type==="api_key";},
	  get providerRegions() {return this.selectedProvider.regions || ["cn","global"];},
	  get credentialExample() {
		if(this.form.provider==="trae-intl")return '{"region":"global","access_token":"本人国际令牌","refresh_token":"本人国际刷新令牌","uid":"本人账号ID","referer_origin":"https://work.trae.ai","timezone":"Asia/Shanghai","models":["auto"]}';
		if(this.form.provider==="qoder")return '{"auth":{"accessToken":"本人令牌","refreshToken":"drt-本人刷新令牌","region":"cn"},"account":{"uid":"本人账号ID"}}';
		if(this.form.provider==="trae")return '{"access_token":"本人令牌","uid":"本人账号ID","machine_id":"原设备标识","device_id":"原设备ID","variant":"solo","scene":"solo_work_lite"}';
		return '粘贴本人客户端导出的完整 JSON';
	  },

      selectProvider() {
        this.form.auto_checkin=false;
        const p=this.selectedProvider;
        this.form.auth_type=p.auth_types[0];
        this.form.api_base_url=p.default_base_url || "";
		this.form.endpoint_url=p.default_base_url || "";this.form.workspace_id="";
        this.form.api_key="";this.form.credential="";this.form.api_models="";
        this.form.project_id="";this.form.client_id="";this.form.client_secret="";
		this.form.trae_variant="solo";this.form.trae_scene="solo_work_lite";
        const selected=this.form.provider;
        const region=p.default_region || (["workbuddy","deepseek"].includes(selected)?"cn":"global");
        // 先更新地区选项，再更新选中值，防止单地区渠道沿用上一个渠道的空选中状态。
        nextTick(() => { if(this.form.provider===selected)this.form.region=region; });
        this.form.drop_system_prompt=this.form.provider==="workbuddy";
      },

	  selectEndpoint() {
		const endpoint=(this.selectedProvider.endpoints || []).find(e=>e.url===this.form.endpoint_url);
		if(endpoint){this.form.api_base_url=endpoint.url;this.form.region=endpoint.region;}
	  },

      async importCredentialFile(ev) {
        const file=ev.target.files && ev.target.files[0];if(!file)return;
        try {
          if(file.size>1024*1024)throw new Error("凭证文件不能超过 1 MB");
          const text=await file.text();let parsed;try{parsed=JSON.parse(text);}catch{throw new Error("凭证文件不是合法 JSON，请检查文件格式");}
          if(!parsed || typeof parsed!=="object" || Array.isArray(parsed))throw new Error("请选择 JSON 凭证对象");
          // 导入文件先验证会进入界面的字段，避免错误类型让表单渲染中断。
          for(const field of ["base_url","project_id"])if(parsed[field]!=null && typeof parsed[field]!=="string")throw new Error("凭证中的地址和项目 ID 必须是文本");
          if(parsed.models!=null && (!Array.isArray(parsed.models) || parsed.models.some(model=>typeof model!=="string")))throw new Error("凭证中的模型列表必须是文本数组");
          const kind=RPAccounts.credentialKind(parsed);
          if(!this.selectedProvider.auth_types.includes(kind.auth_type))throw new Error("该文件凭证类型与所选渠道不符，请选择对应的 API 密钥或登录态渠道");
          const inferredRegion=RPAccounts.region(parsed,this.form.provider);
          if(inferredRegion && !this.providerRegions.includes(inferredRegion))throw new Error("该文件地区与所选渠道不符，请改选对应的国内或国际渠道");
          this.form.credential=text;this.form.auth_type=kind.auth_type;
          if(kind.auth_type==="api_key"){this.form.api_key=kind.key;this.form.api_base_url=parsed.base_url || this.form.api_base_url;}
          else this.form.api_key="";
          if(inferredRegion)this.form.region=inferredRegion;
          if(parsed.project_id)this.form.project_id=parsed.project_id;
          if(Array.isArray(parsed.models))this.form.api_models=parsed.models.join(", ");
		  if(["qoder","trae","trae-intl"].includes(this.form.provider)) {
			const auth=parsed.auth || parsed;
			if(this.form.provider==="trae") {
			  this.form.trae_variant=auth.variant || parsed.variant || "solo";
			  this.form.trae_scene=auth.scene || parsed.scene || "solo_work_lite";
			}
			const region=auth.region || parsed.region;
			if(this.form.provider==="trae-intl" && ["US","US-East","SG"].includes(region))this.form.region="global";
		  }
          toast("凭证已读入表单，点击创建后才会保存");
        } catch(e) {reportErr(e,"读取凭证失败");} finally {ev.target.value="";}
      },

      async reload(options={}) {
        const vm = this;
        vm.loading = true;
        vm.error = "";
        try {
          const [r,p] = await Promise.all([RP.listAccounts(),RP.listProviders()]);
          const savedQuota=new Map(vm.quotaBase===RP.baseUrl()?vm.list.map(a=>[a.id,a.quota]):[]);
          vm.list = (r.data || []).map(a=>({...a,quota:savedQuota.get(a.id)}));
          vm.providers=p.data || [];
        } catch (e) {
          vm.error = e.message;
        } finally {
          vm.loading = false;
        }
        try {vm.checkinSchedule=await RP.checkinSchedule();vm.scheduleError="";}
        catch {vm.checkinSchedule=null;vm.scheduleError="暂时无法读取后台领取计划，可刷新重试。";}
        vm.refreshQuotas(!!options.refreshQuota);
      },

      blankForm,

      openCreate() {
        // Always start from defaults so a previous attempt's values or a stale
        // half-filled form cannot leak into a new account.
        this.form = blankForm();
        this.createOpen = true;
      },

      resetForm() {
        this.form = blankForm();
        this.createOpen = false;
      },

      // API 接入提供独立入口，直接展示中转地址、密钥和模型表单。
      openAPICreate() {
        this.openCreate();
        this.form.provider = "openai-compatible";
        this.selectProvider();
      },

      async create() {
        const vm = this;
        if (vm.creating) return;
        vm.creating = true;
        try {
          const body = {
            name: (vm.form.name || "").trim(),
            provider: vm.form.provider || "workbuddy",
            region: vm.form.region || "cn",
            weight: Number(vm.form.weight) || 0,
            max_inflight: Number(vm.form.max_inflight) || 0,
            priority: Number(vm.form.priority) || 0,
            quota_scope: vm.form.quota_scope,
            quota_group: (vm.form.quota_group || "").trim(),
            auto_checkin: !!vm.form.auto_checkin,
            drop_system_prompt: !!vm.form.drop_system_prompt,
            enabled: vm.form.enabled !== false,
            auth_type: vm.form.auth_type,
          };
          // The credential is a JSON *string* on this endpoint (see API.md's
          // pitfalls list): the backend stores the export verbatim, so the
          // text is validated as JSON here and passed through unchanged.
          const raw = (vm.form.credential || "").trim();
          if (vm.usesAPIKey) {
            body.auth_type = "api_key";
            body.drop_system_prompt = false;
			const models=vm.form.api_models.split(/[\s,]+/).filter(Boolean);
			if(vm.selectedProvider.models_required && !models.length){toast("该平台需要填写实际模型 ID",false);return;}
			let endpoint=vm.form.api_base_url.trim();
			if(endpoint.includes("{workspace_id}")){
			  if(!/^[a-zA-Z0-9-]+$/.test(vm.form.workspace_id.trim())){toast("请填写有效的百炼业务空间 ID",false);return;}
			  endpoint=endpoint.replaceAll("{workspace_id}",vm.form.workspace_id.trim());
			}
            body.credential = JSON.stringify({base_url: endpoint, api_key: vm.form.api_key.trim(), models});
          }
          if (raw && !vm.usesAPIKey) {
            try {
              JSON.parse(raw);
            } catch {
              toast("凭证不是合法的 JSON，已取消创建", false);
              return;
            }
            const credential=JSON.parse(raw);
			if(!credential || typeof credential!=="object" || Array.isArray(credential)){toast("凭证须为 JSON 对象",false);return;}
			if(["qoder","trae","trae-intl"].includes(body.provider)) {
			  if(!vm.providerRegions.includes(body.region)){toast("所选渠道不接受该地区的登录态，请改选对应渠道",false);return;}
			  if(!credential.region && !(credential.auth && credential.auth.region))credential.region=body.region;
			  if(body.provider==="trae"){credential.variant=vm.form.trae_variant;credential.scene=vm.form.trae_scene;}
			}
            if(body.provider!=="workbuddy") {
              const models=vm.form.api_models.split(/[\s,]+/).filter(Boolean);
              if(models.length)credential.models=models;
              if(body.provider==="gemini") {
                credential.mode="code_assist";
                credential.base_url="https://cloudcode-pa.googleapis.com/v1internal";
                if(vm.form.project_id.trim())credential.project_id=vm.form.project_id.trim();
                if(vm.form.client_id.trim())credential.client_id=vm.form.client_id.trim();
                if(vm.form.client_secret.trim())credential.client_secret=vm.form.client_secret.trim();
              }
              body.drop_system_prompt=false;
            }
            body.credential = JSON.stringify(credential);
          }
          if(body.provider!=="workbuddy" && !body.credential) {toast("请填写 API 密钥或导入本人客户端凭证",false);return;}
          const created = await RP.createAccount(body);
          vm.resetForm();
          toast("账号已创建：" + created.id);
          await vm.reload();
        } catch (e) {
          // 非法凭证在建号前拒绝；失败不会留下可调度账号。
          if (e.code === "invalid_credential") {
            reportErr(e, "凭证被拒绝，请修正后重新创建");
          } else {
            reportErr(e, "创建失败");
          }
          await vm.reload();
        } finally {
          vm.creating = false;
        }
      },

      async toggleEnabled(acc, ev) {
        const vm = this;
        try {
          await RP.setAccountEnabled(acc.id, ev.target.checked);
          await vm.reload();
          toast(acc.name + (ev.target.checked ? " 已启用" : " 已停用"));
        } catch (e) {
          ev.target.checked = !ev.target.checked; // 还原
          reportErr(e, "切换失败");
        }
      },

      // 直接在列表保存账号的每日领取开关；失败还原，不擅自启用账号。
      async toggleAutoCheckin(acc, ev) {
        const previous=!!acc.auto_checkin;
        if(this.checkinSaving[acc.id] || !acc.checkin?.supported){ev.target.checked=previous;return;}
        this.checkinSaving[acc.id]=true;
        try {
          const updated=await RP.patchAccount(acc.id,{auto_checkin:ev.target.checked});
          acc.auto_checkin=!!updated.auto_checkin;
          toast(acc.auto_checkin ? "每日自动领取已开启" : "每日自动领取已关闭");
        } catch(e) {ev.target.checked=previous;reportErr(e,"自动领取保存失败");}
        finally {this.checkinSaving[acc.id]=false;}
      },

      // ---- 浏览器登录添加账号 ----
      //
      // 不用手抄 token：后端拿授权链接 → 用户在浏览器登录 → 前端轮询到凭证
      // 后账号自动创建并启用。三条要点：
      //   1. 轮询必须能停（取消 / 超时 / 离开页面），否则一个废弃的登录会
      //      永远每秒打一次后端；
      //   2. 组件销毁时也要停，否则切页面后定时器仍在跑；
      //   3. 失败要能重试，不能把面板锁死。
      get loginProviders() {return this.providers.filter(p=>p.browser_login);},
      get loginRegions() {const p=this.providers.find(p=>p.id===this.login.form.provider);return p?.regions || ["cn","global"];},
      get selectedCheckin() {return this.selectedProvider.checkin_by_region?.[this.form.region] || this.selectedProvider.checkin || {supported:false,note:"无需签到"};},
      selectLoginProvider() {const p=this.providers.find(p=>p.id===this.login.form.provider);this.login.form.region=p?.default_region || "global";this.login.form.client_id="";this.login.form.client_secret="";this.login.form.project_id="";this.login.form.redirect_uri="";},
      login: {
        open:false,step:"form",starting:false,authUrl:"",loginId:"",error:"",done:false,secondsLeft:0,
        mode:"device",userCode:"",callbackURI:"",callbackInput:"",completing:false,
        form:{provider:"workbuddy",region:"cn",name:"",models:"",client_id:"",client_secret:"",project_id:"",redirect_uri:""},
        _timer:null,_deadline:0,_interval:3000,_polling:false,_epoch:0,
        openPanel() {
          this.close();this.open=true;this.step="form";this.error="";this.done=false;this.authUrl="";this.userCode="";this.callbackInput="";
          this.form={provider:"workbuddy",region:"cn",name:"",models:"",client_id:"",client_secret:"",project_id:"",redirect_uri:""};
        },
        close() {
          this._epoch++;this.stopPolling();if(this.loginId&&!this.done)RP.cancelLogin(this.loginId).catch(()=>{});
          this.open=false;this.loginId="";this.form.client_secret="";this.callbackInput="";this.starting=false;this._polling=false;
        },
        stopPolling(){if(this._timer){clearInterval(this._timer);this._timer=null;}},
        startPolling(){this.stopPolling();this._timer=setInterval(()=>this.poll(),this._interval);this.poll();},
        async poll() {
          if(!this.loginId||this.done||this._polling)return;
          const id=this.loginId,epoch=this._epoch;this.secondsLeft=Math.max(0,Math.ceil((this._deadline-Date.now())/1000));
          if(Date.now()>this._deadline){this.stopPolling();this.error="授权已过期，请取消后重新开始";RP.cancelLogin(id).catch(()=>{});return;}
          this._polling=true;
          try {
            const r=await RP.loginStatus(id);if(this._epoch!==epoch||this.loginId!==id)return;
            if(r.status==="done"){this.stopPolling();this.done=true;this.step="done";this.callbackInput="";toast("账号已添加并保存登录态");await app.accounts.reload();return;}
            if(r.status==="expired"||r.status==="failed"){this.stopPolling();this.error=r.message || "授权已失效，请重新开始";return;}
            this.error=r.last_error || "";
          }catch(e){if(this._epoch!==epoch||this.loginId!==id)return;if(e.status===404)this.stopPolling();this.error=e.message || "等待授权失败";}
          finally{if(this._epoch===epoch)this._polling=false;}
        },
      },
      async openLogin(){if(!this.providers.length)await this.reload();this.login.openPanel();},
      closeLogin(){this.login.close();},
      async completeLogin() {
        const L=this.login,id=L.loginId,epoch=L._epoch;if(L.completing||!L.callbackInput.trim())return;L.completing=true;L.error="";
        try{await RP.completeLogin(id,L.callbackInput.trim());if(L._epoch!==epoch)return;L.callbackInput="";await L.poll();}
        catch(e){if(L._epoch===epoch)L.error=e.message;}finally{if(L._epoch===epoch)L.completing=false;}
      },
      async startLogin() {
        const L=this.login,epoch=L._epoch;if(L.starting)return;L.starting=true;L.error="";
        if(L.form.provider==='gemini' && (!L.form.client_id.trim() || !L.form.project_id.trim())){L.error="请填写自己的 Google OAuth 客户端 ID 和 Code Assist 项目 ID";L.starting=false;return;}
        try {
          const r=await RP.startLogin({...L.form,models:L.form.models.split(/[\s,]+/).filter(Boolean)});
          if(L._epoch!==epoch||!L.open){RP.cancelLogin(r.login_id).catch(()=>{});return;}
          // 授权入口来自上游，不能允许 javascript 等可执行地址。
          const authURL=new URL(r.auth_url);if(!['https:','http:'].includes(authURL.protocol) || authURL.username || authURL.password){RP.cancelLogin(r.login_id).catch(()=>{});throw new Error('授权服务返回了无效的登录地址');}
          L.form.client_secret="";L.loginId=r.login_id;L.authUrl=authURL.href;L.mode=r.mode || "device";L.userCode=r.user_code || "";L.callbackURI=r.callback_url || "";
          L.step="wait";L._interval=Math.max(2000,Number(r.poll_interval_seconds || 3)*1000);L._deadline=Date.now()+Number(r.expires_in_seconds || 600)*1000;
          L.startPolling();window.open(L.authUrl,"_blank","noopener,noreferrer");
        }catch(e){if(L._epoch===epoch)L.error=e.message;}finally{if(L._epoch===epoch)L.starting=false;}
      },

      confirmDelete(acc) {
        const vm = this;
        app.modal.open = true;
        app.modal.wide = false;
        app.modal.title = "删除账号";
        app.modal.html =
          '<p class="mt0">删除 <b>' + esc(acc.name || acc.id) + "</b>（" + esc(acc.id) +
          "）？</p>" +
          '<p class="dim" style="font-size:12.5px">凭证与调度状态将一并移除，不可恢复。</p>';
        app.modal.footer = {
          confirmText: "删除",
          danger: true,
          onConfirm: async () => {
            try {
              await RP.deleteAccount(acc.id);
              closeModal();
              toast("已删除 " + (acc.name || acc.id));
              vm.reload();
            } catch (e) {
              closeModal();
              reportErr(e, "删除失败");
            }
          },
        };
      },
    });
  }

  // ===== 账号详情 =====
  function newAccountDetailVM() {
    // NOTE: methods here capture `vm` from the enclosing scope rather than
    // using the `const vm = this` pattern. petite-vue wraps the object in a
    // reactive proxy, and reading `this` inside an async method that was
    // invoked through the template can resolve to the proxy before all fields
    // are installed, which surfaces as "Cannot access 'n' before
    // initialization". Using the factory's own reference is stable.
    const vm = reactive({
      loading: true,
      error: "",
      acc: null,
      armorList: null,
      quota: null,
      quotaBusy: false,
      catalog: null,
      catalogBusy: false,
      quotaLabel(){return RPQuota.label(vm.quota);},
      quotaNumber(value){return RPQuota.format(value);},
      async refreshQuota(refresh=false){
        if(vm.quotaBusy || !vm.acc)return;vm.quotaBusy=true;const id=vm.acc.id,base=RP.baseUrl();
        try{const result=await RP.getAccountQuota(id,refresh);if(vm.acc?.id===id && RP.baseUrl()===base)vm.quota=result;}
        catch{if(vm.acc?.id===id && RP.baseUrl()===base)vm.quota={state:"error",message:"额度读取失败，请刷新重试"};}
        finally{vm.quotaBusy=false;if(vm.acc && vm.acc.id!==id)vm.refreshQuota();}
      },
      async refreshCatalog(){
        if(vm.catalogBusy || !vm.acc)return;vm.catalogBusy=true;const id=vm.acc.id,base=RP.baseUrl();
        try{const result=await RP.getAccountCatalog(id);if(vm.acc?.id!==id || RP.baseUrl()!==base)return;vm.catalog=result;if(result.state==="ok"){vm.acc.models=result.models;if(app.accounts)await app.accounts.reload();}}
        catch{if(vm.acc?.id===id && RP.baseUrl()===base)vm.catalog={state:"error",message:"上游型号查询失败，已保留原目录"};}
        finally{vm.catalogBusy=false;}
      },
	  checkinBusy: false,
	  async checkin() {
		if(vm.checkinBusy || !vm.acc?.checkin?.supported)return;
		vm.checkinBusy=true;
		try {const r=await RP.checkinAccount(vm.acc.id);toast(r.message || "已查询领取结果");await vm.reload();if(app.accounts)await app.accounts.reload();}
		catch(e){reportErr(e,"领取失败");}finally{vm.checkinBusy=false;}
	  },

      // 凭证上传
      credOpen: false,
      credText: "",
      credSaving: false,

      // 编辑期间保留表单，自动刷新不会覆盖尚未保存的输入。
      configEdit: false,
      configSaving: false,
      configForm: {},
      openConfig() {
        const a = vm.acc;
        vm.configForm = {name:a.name, max_inflight:a.max_inflight, priority:a.priority, weight:a.weight, quota_scope:a.quota_scope, quota_group:a.quota_group || "", quota_expires_at:a.quota_expires_at && !a.quota_expires_at.startsWith("0001") ? a.quota_expires_at : "", auto_checkin:!!a.checkin?.supported && a.auto_checkin, drop_system_prompt:a.drop_system_prompt};
        vm.configEdit = true;
      },
      async saveConfig() {
        if (vm.configSaving) return;
        vm.configSaving = true;
        try {
          const f = vm.configForm;
          const expires = f.quota_expires_at.trim();
          if (expires && !Number.isFinite(Date.parse(expires))) throw new Error("额度到期时间须为有效日期，例如 2026-10-31T23:59:59+08:00");
          await RP.patchAccount(vm.acc.id, {...f, max_inflight:Number(f.max_inflight), priority:Number(f.priority), weight:Number(f.weight), quota_expires_at:expires ? new Date(expires).toISOString() : "0001-01-01T00:00:00Z"});
          vm.configEdit = false;
          await vm.reload();
          toast("号池配置已保存并立即生效");
        } catch (e) { reportErr(e, "保存号池配置失败"); }
        finally { vm.configSaving = false; }
      },

      // 破甲（按模型查映射，无账号级设置）
      resolveModel: "",
      resolveResult: null,
      resolveBusy: false,

      async init(id) {
        if (!id) {
          vm.error = "缺少账号 ID";
          vm.loading = false;
          return;
        }
        vm.loading = true;
        vm.error = "";
        vm.acc = null;
        vm.armorList = null;
        vm.quota=null;vm.catalog=null;
        vm.resolveResult = null;
        try {
          // The account is required; the preset list is optional (armor may be
          // unconfigured), so a failure there must not hide the account.
          const account = await RP.getAccount(id);
          vm.acc = account;
          vm.refreshQuota();
          vm.resolveModel = (account.models && account.models[0]) || "";
          try {
            vm.armorList = await RP.listArmor();
          } catch {
            vm.armorList = { enabled: false, data: [] };
          }
        } catch (e) {
          vm.error = e.message;
        } finally {
          vm.loading = false;
        }
      },

      async reload() {
        await vm.init(app.routeParam);
      },

      async toggleEnabled(ev) {
        try {
          vm.acc = await RP.setAccountEnabled(vm.acc.id, ev.target.checked);
          toast((vm.acc.name || vm.acc.id) + (vm.acc.enabled ? " 已启用" : " 已停用"));
        } catch (e) {
          ev.target.checked = !ev.target.checked;
          reportErr(e, "切换失败");
        }
      },

      openCred() {
        vm.credOpen = true;
        vm.credText = "";
      },

      async saveCred() {
        const raw = (vm.credText || "").trim();
        if (!raw) {
          toast("请粘贴凭证 JSON", false);
          return;
        }
        let parsed;
        try {
          parsed = JSON.parse(raw);
        } catch {
          toast("凭证不是合法的 JSON", false);
          return;
        }
        vm.credSaving = true;
        try {
          // payload is sent as the parsed object on this endpoint (unlike
          // account creation, where the same data goes as a JSON string).
          const res = await RP.putCredential(vm.acc.id, parsed);
          vm.credOpen = false;
          vm.credText = "";
          toast("凭证已保存（格式 " + (res && res.format ? res.format : "ok") + "）");
          await vm.reload();
        } catch (e) {
          reportErr(e, "凭证被拒绝");
        } finally {
          vm.credSaving = false;
        }
      },

      // There is no saveArmor: the preset is selected from the requested model,
      // so an account has nothing armure-related to configure.

      async doResolve() {
        const model = (vm.resolveModel || "").trim();
        if (!model) return;
        vm.resolveBusy = true;
        vm.resolveResult = null;
        try {
          vm.resolveResult = await RP.resolveArmor(model);
        } catch (e) {
          vm.resolveResult = { error: e.message };
        } finally {
          vm.resolveBusy = false;
        }
      },
    });
    return vm;
  }

  // ===== 破甲预设库 =====
  function newArmorVM() {
    return reactive({
      loading: true,
      error: "",
      info: null, // {enabled, default, families, data}
      openPreset: null, // 当前查看正文的预设

      importBusy: false,
      importForm: {name:"", family:"", description:"", body:""},
      pack: null,
      packMode: "merge",
      packPreview: null,
      packBusy: false,
      packError: "",
      downloadPack(pack, label) {
        const blob=new Blob([JSON.stringify(pack,null,2)+'\n'],{type:'application/json'});
        const url=URL.createObjectURL(blob), link=document.createElement('a');
        link.href=url;link.download=label+'-'+new Date().toISOString().replace(/[:.]/g,'-')+'.json';
        document.body.appendChild(link);link.click();link.remove();setTimeout(()=>URL.revokeObjectURL(url),1000);
      },
      async exportPack() {
        if(this.packBusy)return;this.packBusy=true;
        try {this.downloadPack(await RP.exportArmorPack(),'relaypool-armor-backup');toast('用户预设和模型绑定已导出');}
        catch(e){reportErr(e,'导出失败');}finally{this.packBusy=false;}
      },
      async readPack(ev) {
        this.pack=null;this.packPreview=null;this.packError='';
        const file=ev.target.files?.[0];if(!file)return;
        try {
          if(file.size>10*1024*1024)throw new Error('预设包不能超过 10 MB');
          const pack=JSON.parse(await file.text());
          if(pack?.format!=='relaypool.armor-pack' || pack.version!==1)throw new Error('请选择 relaypool 预设包 JSON');
          this.pack=pack;await this.previewPack();
        }catch(e){this.packError=e.message;}finally{ev.target.value='';}
      },
      async previewPack() {
        this.packPreview=null;this.packError='';if(!this.pack || this.packBusy)return;this.packBusy=true;
        try {this.packPreview=await RP.previewArmorPack(this.pack,this.packMode);}
        catch(e){this.packError=e.message;}finally{this.packBusy=false;}
      },
      async installPack() {
        if(!this.packPreview || this.packBusy)return;this.packBusy=true;this.packError='';
        try {
          const previous=await RP.exportArmorPack();
          if(previous.state_sha256!==this.packPreview.base_sha256)throw new Error('配置已变化，请重新预览');
          // 提交前下载现有配置；服务器仍以指纹检查防止另一窗口同时修改。
          this.downloadPack(previous,'relaypool-armor-before-import');
          await RP.importArmorPack(this.pack,this.packMode,previous.state_sha256);
          this.pack=null;this.packPreview=null;await this.reload();toast('预设包已导入，原配置备份已下载；密钥权限未改变');
        }catch(e){this.packError=e.message;this.packPreview=null;}finally{this.packBusy=false;}
      },
      bindingForm: {model:"", preset:""},
      bindingBusy: false,
      bindingResult: null,
      previewModel: "",
      previewResult: null,
      previewBusy: false,
      get curated() { return (this.info?.data || []).filter(p=>p.auto_selectable); },
      get customPresets() { return (this.info?.data || []).filter(p=>p.imported); },
      async resolveModel() {
        if(!this.previewModel.trim() || this.previewBusy)return;
        this.previewResult=null;
        this.previewBusy=true;
        try { this.previewResult=await RP.resolveArmor(this.previewModel.trim()); } catch(e){reportErr(e,'预览匹配失败');} finally{this.previewBusy=false;}
      },
      get bindings() { return Object.entries(this.info?.model_bindings || {}).map(([model,preset])=>({model,preset})); },
      async readLocalFile(ev) {
        const vm=this;const file=ev.target.files?.[0];if(!file)return;
        if(file.size>1048576){toast("提示文件不能超过 1 MB",false);return;}
        vm.importForm.body=await file.text();
        vm.importForm.name=file.name.replace(/\.[^.]+$/,'').toLowerCase().replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'').slice(0,70) || 'custom-preset';
      },
      async importPreset() {
        const vm=this;if(vm.importBusy)return;vm.importBusy=true;
        try {
          const body={family:vm.importForm.family.trim(),description:vm.importForm.description.trim()};
          body.body=vm.importForm.body;
          const result=await RP.importArmor(vm.importForm.name.trim(),body);
          vm.bindingForm.preset=result.name;
          await vm.reload();toast("预设已导入，请绑定到实际模型后使用");
        } catch(e){reportErr(e,"导入失败");}
        finally {vm.importBusy=false;}
      },
      async saveBinding() {
        const vm=this;if(vm.bindingBusy)return;vm.bindingBusy=true;vm.bindingResult=null;
        try {await RP.bindArmor(vm.bindingForm.model.trim(),vm.bindingForm.preset);vm.bindingResult=await RP.resolveArmor(vm.bindingForm.model.trim());await vm.reload();toast("模型选择已保存，重启后仍生效");}
        catch(e){reportErr(e,"保存模型选择失败");}
        finally {vm.bindingBusy=false;}
      },
      async removePreset(name) {
        try {await RP.deleteArmor(name);await this.reload();toast("导入预设已删除");}catch(e){reportErr(e,"删除失败");}
      },

      async reload() {
        const vm = this;
        vm.loading = true;
        vm.error = "";
        vm.openPreset = null;
        try {
          vm.info = await RP.listArmor();

        } catch (e) {
          vm.error = e.message;
        } finally {
          vm.loading = false;
        }
      },

      // 按 family 分组（空 family 归入 "（无族）"）
      get groups() {
        if (!this.info || !this.info.data) return [];
        const map = new Map();
        for (const p of this.info.data) {
          const fam = p.family || "";
          const key = fam || "（无族 · 仅可点名）";
          if (!map.has(key)) map.set(key, []);
          map.get(key).push(p);
        }
        // 自动选的排前面，同组内按名字
        const out = [];
        for (const [key, arr] of map) {
          arr.sort((a, b) => (b.auto_selectable - a.auto_selectable) || a.name.localeCompare(b.name));
          out.push({ family: key, presets: arr });
        }
        out.sort((a, b) => a.family.localeCompare(b.family));
        return out;
      },

      async open(name) {
        const vm = this;
        try {
          const p = await RP.getArmor(name);
          vm.openPreset = p;
        } catch (e) {
          reportErr(e, "读取预设失败");
        }
      },
    });
  }

  // ===== 密钥 =====
  function newKeysVM() {
    return reactive({
      loading: true,
      error: "",
      list: [],
      newName: "",
      newScope: "inference",
      permissions: {armor_allowed:false,armor_enabled:false,all_accounts:true,account_ids:[],all_models:true,models:[]},
      toggling: {},
      editID: "",editOriginal:null,options:{accounts:[],models:[]},modelQuery:"",extraModels:"",formError:"",optionsError:"",
      get filteredModels(){const q=this.modelQuery.trim().toLowerCase();return this.options.models.filter(m=>(!q || m.id.toLowerCase().includes(q)) && !/\/(cn|global)$/.test(m.id));},
      get missingAccounts(){return this.permissions.account_ids.filter(id=>!this.options.accounts.some(a=>a.id===id));},
      resetForm(){this.editID="";this.editOriginal=null;this.newName="";this.newScope="inference";this.permissions={armor_allowed:false,armor_enabled:false,all_accounts:true,account_ids:[],all_models:true,models:[]};this.extraModels="";this.modelQuery="";this.formError="";},
      armorPermissionChanged(){if(!this.permissions.armor_allowed)this.permissions.armor_enabled=false;},
      edit(k){this.editID=k.id;this.editOriginal={name:k.name,scope:k.scope,permissions:JSON.parse(JSON.stringify(k.permissions))};this.newName=k.name;this.newScope=k.scope;this.permissions=JSON.parse(JSON.stringify(k.permissions));this.extraModels=this.permissions.models.filter(id=>!this.options.models.some(m=>m.id===id)).join("\n");this.permissions.models=this.permissions.models.filter(id=>this.options.models.some(m=>m.id===id));this.modelQuery="";this.formError="";document.getElementById("key-editor")?.scrollIntoView({behavior:"smooth",block:"start"});},
      // 只比较有效授权集合，列表顺序和隐藏的旧选项不构成权限修改。
      permissionSignature(p){return JSON.stringify([!!p.armor_allowed,!!p.armor_enabled,!!p.all_accounts,p.all_accounts?[]:[...(p.account_ids||[])].sort(),!!p.all_models,p.all_models?[]:[...(p.models||[])].sort()]);},
      creating: false,
      lastCreated: null, // {id, name, key}

      async reload() {
        const vm = this;
        vm.loading = true;
        vm.error = "";
        try {
          const r = await RP.listKeys();
          vm.list = r.data || [];
          vm.optionsError="";try{vm.options=await RP.keyOptions();}catch(e){vm.optionsError="可选账号和模型读取失败："+e.message;}
        } catch (e) {
          vm.error = e.message;
        } finally {
          vm.loading = false;
        }
      },

      async create() {
        const vm=this;if(vm.creating)return;vm.creating=true;vm.formError="";
        try{
          const p=JSON.parse(JSON.stringify(vm.permissions));
          if(!p.armor_allowed)p.armor_enabled=false;
          p.models=p.all_models?[]:[...new Set([...p.models,...vm.extraModels.split(/[\n,，]/).map(v=>v.trim()).filter(Boolean)])];
          p.account_ids=p.all_accounts?[]:p.account_ids;
          if(vm.editID){
            // 表单仅提交实际改动，名称修改不能带回编辑开始时的旧授权。
            const changes={},original=vm.editOriginal;
            if(vm.newName.trim()!==original.name)changes.name=vm.newName.trim();
            if(vm.newScope!==original.scope)changes.scope=vm.newScope;
            if(vm.permissionSignature(p)!==vm.permissionSignature(original.permissions))changes.permissions=p;
            if(Object.keys(changes).length)await RP.updateKey(vm.editID,changes);
            toast(Object.keys(changes).length?"密钥修改已保存，下一次请求生效":"密钥配置没有变化");
          }
          else{vm.lastCreated=await RP.createKey(vm.newName.trim(),vm.newScope,p);}
          vm.resetForm();await vm.reload();
          if(app.chat)await app.chat.reload();
        }catch(e){vm.formError=e.message;}finally{vm.creating=false;}
      },

      dismissCreated() {
        this.lastCreated = null;
      },

      // 单独保存开关，不用重新创建密钥，也不会覆盖账号和型号白名单。
      async toggleArmor(k) {
        if(this.toggling[k.id] || !k.permissions.armor_allowed)return;
        this.toggling[k.id]=true;
        try {
          const saved=await RP.setKeyArmor(k.id,!k.permissions.armor_enabled);
          const index=this.list.findIndex(item=>item.id===k.id);
          if(index>=0)this.list[index]=saved;
          if(this.editID===k.id){this.permissions.armor_enabled=this.permissions.armor_allowed && saved.permissions.armor_enabled;this.editOriginal.permissions.armor_enabled=saved.permissions.armor_enabled;}
          toast(saved.permissions.armor_enabled?"该密钥已开启破甲":"该密钥已关闭破甲，使用许可保留");
          if(app.chat)await app.chat.reload();
        }catch(e){toast("开关保存失败："+e.message,false);}finally{delete this.toggling[k.id];}
      },

      async copyCreated() {
        const vm = this;
        try {
          await navigator.clipboard.writeText(vm.lastCreated.key);
          toast("已复制到剪贴板");
        } catch {
          toast("复制失败，请手动选择", false);
        }
      },

      confirmDelete(k) {
        const vm = this;
        app.modal.open = true;
        app.modal.wide = false;
        app.modal.title = "删除密钥";
        app.modal.html =
          '<p class="mt0">删除密钥 <b>' + esc(k.name || k.id) + "</b>（前缀 <code class='inline'>" +
          esc(k.key_prefix) + "</code>）？</p>" +
          '<p class="dim" style="font-size:12.5px">正在使用该 Key 的客户端将立即失去访问权。</p>';
        app.modal.footer = {
          confirmText: "删除",
          danger: true,
          onConfirm: async () => {
            try {
              await RP.deleteKey(k.id);
              closeModal();
              toast("已删除 " + (k.name || k.id));
              vm.reload();
            } catch (e) {
              closeModal();
              if (e.code === "invalid_request") {
                // 删最后一个 Key 是有意的保护，不是 bug
                toast("后端拒绝：" + e.message, false);
              } else {
                reportErr(e, "删除失败");
              }
            }
          },
        };
      },
    });
  }

  // ===== 请求日志 =====
  function newLogsVM() {
    return reactive({
      loading: true,
      error: "",
      list: [],
      total: 0,
      limit: 50,
      offset: 0,
      filter: { account_id: "", provider: "", status: "" },
      expanded: null, // 展开的请求 id
      attempts: null, // 展开行的换号明细
      attemptsLoading: false, attemptsError: "", listRevision: 0, detailRevision: 0,
      providers: [], accounts: [], optionsError: "",
      get providerOptions(){const items=new Map(this.providers.map(p=>[p.id,{id:p.id,label:p.label||p.id}]));for(const a of this.accounts)if(!items.has(a.provider))items.set(a.provider,{id:a.provider,label:a.provider});for(const r of this.list)if(r.provider&&!items.has(r.provider))items.set(r.provider,{id:r.provider,label:r.provider});return [...items.values()];},

      async reload(reset, requestedOffset=this.offset) {
        const vm = this;
        const revision=++vm.listRevision,offset=reset?0:requestedOffset;
        vm.loading = true;
        vm.error = "";
        try {
          const params={
            limit: vm.limit,
            offset,
            account_id: vm.filter.account_id,
            provider: vm.filter.provider,
            status: vm.filter.status,
          };
          const results=await Promise.allSettled([RP.listRequests(params),RP.listProviders(),RP.listAccounts()]);
          if(revision!==vm.listRevision)return;
          if(results[0].status==='rejected')throw results[0].reason;
          const r=results[0].value;
          vm.optionsError=results.slice(1).some(v=>v.status==='rejected')?'筛选目录读取失败，已保留此前选项':'';
          if(results[1].status==='fulfilled')vm.providers=results[1].value.data||[];
          if(results[2].status==='fulfilled')vm.accounts=results[2].value.data||[];
          vm.list = r.data || [];
          vm.offset=offset;vm.detailRevision++;vm.expanded=null;vm.attempts=null;vm.attemptsLoading=false;vm.attemptsError='';
          vm.total = vm.list.length; // 后端不回总数，用本页条数判断翻页
        } catch (e) {
          if(revision===vm.listRevision)vm.error = e.message;
        } finally {
          if(revision===vm.listRevision)vm.loading = false;
        }
      },

      async toggleExpand(req) {
        const vm = this;
        const revision=++vm.detailRevision;
        if (vm.expanded === req.id) {
          vm.expanded = null;
          vm.attempts = null;
          vm.attemptsLoading=false;vm.attemptsError='';
          return;
        }
        vm.expanded = req.id;
        vm.attempts = null;
        vm.attemptsLoading=true;vm.attemptsError='';
        try {
          const r = await RP.listAttempts(req.id);
          if(revision!==vm.detailRevision||vm.expanded!==req.id)return;
          vm.attempts = r.data || [];
        } catch (e) {
          if(revision===vm.detailRevision&&vm.expanded===req.id)vm.attemptsError=e.message;
        }finally{if(revision===vm.detailRevision)vm.attemptsLoading=false;}
      },
      retryAttempts(req){if(this.attemptsLoading)return;this.expanded=null;return this.toggleExpand(req);},

      next() {
        if(!this.loading)return this.reload(false,this.offset+this.limit);
      },
      prev() {
        if(!this.loading)return this.reload(false,Math.max(0,this.offset-this.limit));
      },
    });
  }

  // ===== 设置 =====
  function newSettingsVM() {
    return reactive({
      saved:false,saving:false,error:'',baseVal:RP.base.value,keyVal:RP.key.value,
      get healthURL(){return RP.baseUrl()+'/healthz';},
      get readyURL(){return RP.baseUrl()+'/readyz';},
      reset(){if(this.saving)return;this.baseVal=RP.base.value;this.keyVal=RP.key.value;this.error='';this.saved=false;},
      async save() {
        if(this.saving)return;this.saving=true;this.error='';this.saved=false;
        try{
          const result=await RP.validateConnection(this.baseVal,this.keyVal);
          // 离页或退出后，迟到的验证不得应用新的身份和连接。
          if(app.settings!==this||parseHash().name!=='settings'){toast('已离开连接设置，草稿未应用');return;}
          invalidateConnectionChecks();
          const changed=result.base!==RP.base.value||result.key!==RP.key.value;
          if(changed){
            navGeneration++;
            if(app.chat){app.chat.stop();app.chat.unmounted();}
            if(app.accounts){app.accounts.stopTests();app.accounts.login?.stopPolling();}
            for(const name of ['overview','chat','accounts','accountDetail','armor','keys','logs'])app[name]=null;
          }
          RP.base.value=result.base;RP.key.value=result.key;
          this.baseVal=result.base;this.keyVal=result.key;this.saved=true;
          app.conn.health=result.health;app.conn.access=result.access;app.conn.accessBadge=accessBadge(result.access.mode);app.conn.connError='';app.authed=true;
          toast('连接已验证并保存');
        }catch(e){this.error='验证失败，原连接保持不变：'+e.message;}
        finally{this.saving=false;}
      },
    });
  }

  app._factories = {
    overview: newOverviewVM,
    chat: ()=>RPChat.createVM(reactive,nextTick,toast),
    accounts: newAccountsVM,
    accountDetail: newAccountDetailVM,
    armor: newArmorVM,
    keys: newKeysVM,
    logs: newLogsVM,
    settings: newSettingsVM,
  };

  // 进入某页时确保 vm 存在（懒创建 + 触发加载）
  function ensureVM(name) {
    const key = routes[name];
    if (!key || !app._factories[key]) return;
    if (!app[key]) {
      app[key] = app._factories[key]();
      app[key].autoReload = true;
    }
    // NOTE: this function deliberately does NOT load data. Loading is owned by
    // navigate(), which is the single place that knows which navigation is
    // current; a second loader here is how a stale response used to overwrite a
    // newer page.
  }

  // ---------- 登录门 ----------

  app.gate = reactive({
    keyInput: RP.key.value || "",
    baseInput: "",
    busy: false,
    error: "",
    health: null,
    async submit() {
      const vm = this;
      vm.busy = true;
      vm.error = "";
      // 写入当前浏览器会话，关闭会话后不再长期保留管理密钥。
      RP.key.value = vm.keyInput.trim();
      RP.base.value = vm.baseInput.trim();
      await checkConnection();
      vm.busy = false;
      if (app.authed) {
        // 首次进入概览
        app.overview = null;
        enterApp();
        toast("连接成功");
      } else {
        vm.error = app.conn.connError || "无法验证 Key";
      }
    },
  });

  function enterApp() {
    // navigate() resolves the hash, builds the view model and commits the route
    // once the data is in hand, so it is the single entry point for "show this
    // route".
    navigate();
  }

  // ---------- 启动 ----------

  // gate 里展示探针状态（免鉴权）
  RP.healthz().then((h) => (app.gate.health = h)).catch(() => {});

  // petite-vue 应用。
  //
  // createApp(scope) 把 scope 作为模板的求值上下文（v-if="authed" 读
  // scope.authed）。这里**必须**把 reactive 的 app 本身作为根，不能
  // Object.assign 复制属性——复制出来的是当时的值快照，`authed` 这类基础
  // 类型会脱离响应式，登录成功后 v-if 永远不会翻面。
  //
  // $helpers 通过 app 上的字段提供：helper 挂在 app 上（下面这些赋值）而不是
  // 塞进第二个对象，是因为 createApp 只接受一个根 scope。
  app.$fmt = fmt;
  app.$esc = esc;
  app.$version = "__BUILD_VERSION__";
  app.$buildVersion = "__BUILD_VERSION__";
  app.$buildAt = "__BUILD_AT__";
  app.$base = "http://127.0.0.1:8787";
  app.$armorGroups = function (armorInfo) {
    if (!armorInfo || !armorInfo.data) return [];
    const map = new Map();
    for (const p of armorInfo.data) {
      const key = p.family || "（无族 · 仅可点名）";
      if (!map.has(key)) map.set(key, []);
      map.get(key).push(p);
    }
    const out = [];
    for (const [key, arr] of map) {
      arr.sort((a, b) => (b.auto_selectable - a.auto_selectable) || a.name.localeCompare(b.name));
      out.push({ family: key, presets: arr });
    }
    out.sort((a, b) => a.family.localeCompare(b.family));
    return out;
  };
  app.$resolveSource = function (source) {
    const m = {
      explicit: "点名预设（最高优先级）",
      model: "按请求的模型自动选",
    };
    return m[source] || source || "—";
  };
  app.$proto = function (protocol) {
    const m = {
      "chat.completions": "chat",
      messages: "anthropic",
      responses: "responses",
    };
    return m[protocol] || protocol || "—";
  };
  // $errKind renders a classified failure kind in Chinese. The kinds come from
  // the scheduler's own decision table, so this map is the human side of it.
  app.$errKind = function (kind) {
    const m = {
      quota: "额度耗尽",
      rate_limit: "限流",
      auth: "鉴权失败",
      not_ready: "未就绪",
      unavailable: "不可用",
      invalid_request: "非法请求",
      model_not_available: "模型不存在",
      unknown: "未分类",
      stream_error: "回复中断",
    };
    return m[kind] || kind || "未分类";
  };

  createApp(app).mount("#app");

  // 启动时先探测连接；探针都通不连的 gate 里提示
  (async function boot() {
    await checkConnection();
    if (app.authed) {
      enterApp();
    }
    // When not authenticated the hash is left untouched: the login gate is
    // shown, and navigate() reads the hash again once a key is accepted, so the
    // user still lands on the page they originally asked for.
  })();

  // NOTE: the hashchange listener is registered exactly once, next to navigate()
  // above. A second registration used to live here and caused every navigation
  // to run the route logic twice.

  window.RPBOOT = { enterApp, ensureVM, checkConnection, parseHash, navigate };
})();
