// 额度只展示上游值，不从本地调用日志估算；零余额与没有读到余额分别显示。
(function(){
  "use strict";
  const number=value=>typeof value==="number" && Number.isFinite(value) && value>=0;
  const format=value=>number(value)?value.toLocaleString("zh-CN",{maximumFractionDigits:4}):"未知";
  window.RPQuota={
    format,
    label(snapshot){
      if(!snapshot)return "未查询";
      if(snapshot.state==="loading")return "查询中…";
      if(snapshot.state==="unsupported")return "未接入查询";
      if(snapshot.state==="error")return "查询失败";
      if(snapshot.state!=="ok")return "额度未知";
      const amounts=(snapshot.amounts||[]).filter(a=>number(a.remaining));
      return amounts.length?amounts.map(a=>format(a.remaining)+" "+a.unit).join(" / "):"额度未知";
    }
  };
})();
