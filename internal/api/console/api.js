// relaypool 控制台 — 后端 API 客户端
//
// 约定（见 docs/API.md）：
// - 鉴权：Authorization: Bearer <key>（也接受 x-api-key）
// - 错误信封统一：{"error":{"message","type","code"}}
// - token 字段可能缺失（未知 ≠ 0）
//
// 全部方法返回 {ok, status, data} 或抛异常由调用方决定。

(function () {
  "use strict";

  const LS_KEY = "relaypool.console.key";
  const LS_BASE = "relaypool.console.base";

  // ---- state ----
  // 密钥仅保存在当前会话；首次升级移走旧的长期副本，不打断已有登录。
  let keyValue = "";
  try {
    keyValue = sessionStorage.getItem(LS_KEY) || localStorage.getItem(LS_KEY) || "";
    if (keyValue) sessionStorage.setItem(LS_KEY, keyValue);
    localStorage.removeItem(LS_KEY);
  } catch { /* 存储被禁用时仍可使用内存中的密钥。 */ }
  let baseValue = "";
  try { baseValue = localStorage.getItem(LS_BASE) || ""; } catch { /* 使用当前页面地址。 */ }

  const key = {
    get value() {
      return keyValue;
    },
    set value(v) {
      keyValue = v || "";
      try {
        if (keyValue) sessionStorage.setItem(LS_KEY, keyValue);
        else sessionStorage.removeItem(LS_KEY);
        localStorage.removeItem(LS_KEY);
      } catch { /* 当前页面继续使用内存状态。 */ }
    },
    // Re-read what is in storage right now (e.g. after another tab changed it).
    reload() {
      try { keyValue = sessionStorage.getItem(LS_KEY) || ""; } catch { /* 保留内存状态。 */ }
      try { baseValue = localStorage.getItem(LS_BASE) || ""; } catch { /* 保留当前连接。 */ }
      return keyValue;
    },
  };
  const base = {
    get value() {
      return baseValue;
    },
    set value(v) {
      baseValue = v || "";
      try {
        if (baseValue) localStorage.setItem(LS_BASE, baseValue);
        else localStorage.removeItem(LS_BASE);
      } catch { /* 当前页面继续使用内存中的地址。 */ }
    },
  };

  function defaultBase() {
    // 页面本身就是后端服务的（/console/），同源最省事。
    if (location.protocol.startsWith("http")) {
      return location.origin;
    }
    return "http://127.0.0.1:8787";
  }

  function baseUrl() {
    if (!baseValue) return defaultBase();
    // 去掉末尾斜杠，避免 //admin/...
    return baseValue.replace(/\/+$/, "");
  }

  function saveSettings() {
    // The setters already persist; this exists for callers that want to force
    // both values through in one step.
    key.value = keyValue;
    base.value = baseValue;
  }

  // ---- low-level request ----
  // 使用独立草稿探测目标；验证失败不得改写当前连接或浏览器存储。
  async function validateConnection(draftBase, draftKey) {
    const raw=String(draftBase||'').trim(),credential=String(draftKey||'').trim();
    let target=defaultBase();
    if(raw){
      let url;try{url=new URL(raw);}catch{throw Error('后端地址须为完整的 HTTP 或 HTTPS 地址');}
      if(!['http:','https:'].includes(url.protocol)||url.username||url.password||url.search||url.hash)throw Error('后端地址不能包含用户名、密码、查询参数或片段');
      target=url.href.replace(/\/+$/,'');
    }
    if(!credential)throw Error('请填写目标后端的管理密钥');
    const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),15000);
    const read=async(path,authenticated)=>{
      const response=await fetch(target+path,{headers:authenticated?{Authorization:'Bearer '+credential}:{},signal:controller.signal,redirect:'error',cache:'no-store',credentials:'omit'});
      const value=await response.json();if(!response.ok)throw new ApiError(response.status,value);return value;
    };
    try{const health=await read('/healthz',false),access=await read('/admin/access',true);return {base:raw?target:'',key:credential,health,access};}
    catch(e){if(controller.signal.aborted)throw Error('连接验证超时，原连接保持不变');throw e;}
    finally{clearTimeout(timer);}
  }

  class ApiError extends Error {
    constructor(status, payload) {
      const msg =
        payload && payload.error && payload.error.message
          ? payload.error.message
          : "HTTP " + status;
      super(msg);
      this.status = status;
      this.code = (payload && payload.error && payload.error.code) || "";
      this.payload = payload;
    }
  }

  /**
   * request("GET", "/admin/accounts") -> 解析后的 JSON
   * 失败抛 ApiError（401 时自动进入"未鉴权"状态）。
   */
  async function request(method, path, body, extraHeaders = {}, options = {}) {
    // Pick up a key stored by another tab or an earlier page load, so the very
    // first request after a hash-only navigation still carries a credential.
    if (!keyValue) key.reload();
    const headers = { ...extraHeaders };
    if (keyValue) headers["Authorization"] = "Bearer " + keyValue;
    let payload;
    if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }
    let resp;
    try {
      resp = await fetch(baseUrl() + path, {
        method: method,
        headers: headers,
        body: payload,
        redirect: "error",
        cache: "no-store",
        signal: options.signal,
      });
    } catch (e) {
      // 网络层失败（后端没起 / 地址错）
      const err = new ApiError(0, null);
      err.message = "无法连接后端：" + (e && e.message ? e.message : e);
      err.code = "network_error";
      throw err;
    }
    if (resp.status === 204) return null;
    let data = null;
    const text = await resp.text();
    if (text) {
      try {
        data = JSON.parse(text);
      } catch {
        data = { raw: text };
      }
    }
    if (!resp.ok) throw new ApiError(resp.status, data);
    return data;
  }

  const api = {
    request,
    stream,
    ApiError,
    key,
    base,
    baseUrl,
    saveSettings,
    validateConnection,
    get authed() {
      return !!keyValue;
    },

    // ---- 探针（免鉴权）----
    healthz: () => request("GET", "/healthz"),
    readyz: () => request("GET", "/readyz"),
    version: () => request("GET", "/version"),

    // ---- 访问 ----
    access: () => request("GET", "/admin/access"),

    // ---- 账号 ----
    listAccounts: () => request("GET", "/admin/accounts"),
    listProviders: () => request("GET", "/admin/providers"),
    listArmorSources: () => request("GET", "/admin/armor/sources"),
    listArmorSourceFiles: (id) => request("GET", "/admin/armor/sources/" + encodeURIComponent(id)),
    previewArmorSource: (id, path) => request("GET", "/admin/armor/sources/" + encodeURIComponent(id) + "/preview?path=" + encodeURIComponent(path)),
    importArmor: (name, body) => request("PUT", "/admin/armor/" + encodeURIComponent(name), body),
    deleteArmor: (name) => request("DELETE", "/admin/armor/" + encodeURIComponent(name)),
    bindArmor: (model, preset) => request("PUT", "/admin/armor/bindings", {model, preset}),
    exportArmorPack: () => request("GET", "/admin/armor/pack"),
    previewArmorPack: (pack, mode) => request("POST", "/admin/armor/pack/preview?mode=" + encodeURIComponent(mode), pack),
    importArmorPack: (pack, mode, hash) => request("PUT", "/admin/armor/pack?mode=" + encodeURIComponent(mode), pack, {"If-Match": '"' + hash + '"'}),
    createAccount: (body) => request("POST", "/admin/accounts", body),
    getAccount: (id) => request("GET", "/admin/accounts/" + encodeURIComponent(id)),
    getAccountQuota: (id, refresh=false) => request("GET", "/admin/accounts/" + encodeURIComponent(id) + "/inspect/quota" + (refresh ? "?refresh=1" : "")),
    getAccountCatalog: (id) => request("GET", "/admin/accounts/" + encodeURIComponent(id) + "/inspect/catalog"),
    patchAccount: (id, body) => request("PATCH", "/admin/accounts/" + encodeURIComponent(id), body),
    deleteAccount: (id) => request("DELETE", "/admin/accounts/" + encodeURIComponent(id)),
    setAccountEnabled: (id, enabled) =>
      request("POST", "/admin/accounts/" + encodeURIComponent(id) + "/enabled", { enabled: !!enabled }),
    putCredential: (id, payload) =>
      request("PUT", "/admin/accounts/" + encodeURIComponent(id) + "/credential", { payload }),

    // ---- 浏览器登录加号 ----
    // 三步：start 拿授权链接 → 用户在浏览器登录 → status 轮询拿凭证。
    // 手机/浏览器登录后网关自动建号并启用，无需手工粘贴 token。
    completeLogin: (id, callback_url) => request("POST", "/admin/accounts/login/" + encodeURIComponent(id) + "/complete", {callback_url}),
    checkinAccount: (id) => request("POST", "/admin/accounts/" + encodeURIComponent(id) + "/checkin", {}),
    checkinSchedule: () => request("GET", "/admin/checkin/schedule"),
    startLogin: (body) => request("POST", "/admin/accounts/login/start", body),
    loginStatus: (id) => request("GET", "/admin/accounts/login/" + encodeURIComponent(id)),
    cancelLogin: (id) =>
      request("POST", "/admin/accounts/login/" + encodeURIComponent(id) + "/cancel"),

    // ---- 破甲 ----
    listArmor: () => request("GET", "/admin/armor"),
    getArmor: (name) => request("GET", "/admin/armor/" + encodeURIComponent(name)),
    // resolveArmor reports which preset a model would receive. Selection is by
    // model, so there is no account parameter any more.
    resolveArmor: (model, account = "") => request("GET", "/admin/armor/resolve?model=" + encodeURIComponent(model) + (account ? "&account_id=" + encodeURIComponent(account) : "")),

    // ---- 密钥 ----
    listKeys: () => request("GET", "/admin/keys"),
    createKey: (name, scope = "inference", permissions) => request("POST", "/admin/keys", { name: name || "", scope, ...(permissions ? {permissions} : {}) }),
    updateKey: (id, body) => request("PATCH", "/admin/keys/" + encodeURIComponent(id), body),
    setKeyArmor: (id, enabled) => request("PUT", "/admin/keys/" + encodeURIComponent(id) + "/armor", {enabled}),
    setCurrentKeyArmor: (enabled) => request("PUT", "/v1/key/armor", {enabled}),
    keyOptions: () => request("GET", "/admin/keys/options"),
    keyPermissions: () => request("GET", "/v1/key/permissions"),
    deleteKey: (id) => request("DELETE", "/admin/keys/" + encodeURIComponent(id)),

    // ---- 统计 ----
    // One call returns every aggregate the overview needs, so the cards and the
    // chart can never disagree about the same window.
    stats: (window) => request("GET", "/admin/stats" + (window ? "?window=" + encodeURIComponent(window) : "")),

    // ---- 日志 ----
    listRequests: (params) => {
      params = params || {};
      const q = Object.keys(params)
        .filter((k) => params[k] !== "" && params[k] !== null && params[k] !== undefined)
        .map((k) => k + "=" + encodeURIComponent(params[k]))
        .join("&");
      return request("GET", "/admin/requests" + (q ? "?" + q : ""));
    },
    listAttempts: (reqId) => request("GET", "/admin/requests/" + encodeURIComponent(reqId) + "/attempts"),
  };

  window.RP = api;

  // 共用流读取器：处理跨包中文、CRLF、多行事件及断线，取消会释放连接。
  async function stream(path, body, onEvent, signal, onHeaders) {
    if (!keyValue) key.reload();
    const resp = await fetch(baseUrl() + path, {method:"POST", headers:{"Content-Type":"application/json", "Authorization":"Bearer " + keyValue}, body:JSON.stringify(body), signal, redirect:"error", cache:"no-store"});
    // 错误响应同样带有请求与预设诊断，先读取头再处理错误。
    if (onHeaders) onHeaders(resp.headers);
    if (!resp.ok) {
      let data; try {data=await resp.json();} catch {data=null;}
      throw new ApiError(resp.status,data);
    }
    if (!resp.body || !(resp.headers.get("content-type")||"").includes("text/event-stream")) throw new Error("后端没有返回消息流");
    const reader=resp.body.getReader();
    const decoder=new TextDecoder();
    let buffer="", terminal=false;
    function consume() {
      let match;
      while ((match=/\r?\n\r?\n/.exec(buffer))) {
        const frame=buffer.slice(0,match.index); buffer=buffer.slice(match.index+match[0].length);
        const value=frame.split(/\r?\n/).filter(line=>line.startsWith("data:")).map(line=>line.slice(5).replace(/^ /,"")).join("\n");
        if (!value) continue;
        if(value==="[DONE]"){terminal=true;continue;}
        const event=JSON.parse(value);
        if(event.error)throw new ApiError(502,event);
        if(event.type==="done")terminal=true;
        onEvent(event);
      }
    }
    try {
      for (;;) {
        const {done,value}=await reader.read();
        if(done){buffer+=decoder.decode();consume();break;}
        buffer+=decoder.decode(value,{stream:true});consume();
        if(buffer.length>2*1024*1024)throw new Error("消息流单个事件超过大小限制");
      }
      if (!terminal) throw new Error("连接提前结束，回复可能不完整");
    } finally {await reader.cancel().catch(()=>{});reader.releaseLock();}
  }
})();
