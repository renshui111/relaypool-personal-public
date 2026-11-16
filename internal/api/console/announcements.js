(function () {
  "use strict";
  const empty=()=>({release:"",published_on:"",title:"更新公告",summary:"",changes:[]});
  function parse(value) {
    if(!value||typeof value.release!=="string"||!value.release.trim()||typeof value.published_on!=="string"||
      !/^\d{4}-\d{2}-\d{2}$/.test(value.published_on)||typeof value.title!=="string"||!value.title.trim()||
      typeof value.summary!=="string"||!Array.isArray(value.changes)||!value.changes.length||
      value.changes.some(x=>!x||typeof x.title!=="string"||typeof x.description!=="string"))throw Error("公告内容暂时无法读取");
    return {release:value.release,published_on:value.published_on,title:value.title,summary:value.summary,
      changes:value.changes.map(x=>({title:x.title,description:x.description}))};
  }
  function createVM(reactive,nextTick) {
    let revision=0,shownOnEntry=false,returnFocus=null;
    const identity=()=>RP.baseUrl()+"\n"+RP.key.value;
    const dialog=()=>document.getElementById("release-announcement");
    const vm=reactive({
      data:empty(),loading:false,error:"",
      async openOnEntry(){
        if(shownOnEntry)return;
        shownOnEntry=true;
        await this.show();
      },
      async show(){
        const rev=++revision,who=identity();
        this.loading=true;this.error="";this.data=empty();
        await nextTick();
        if(rev!==revision||who!==identity())return;
        const el=dialog();
        if(!el){this.loading=false;return;}
        if(!el.open){returnFocus=document.activeElement;el.showModal();el.querySelector("[data-announcement-dismiss]")?.focus();}
        try{
          const value=await RP.announcements();
          if(rev===revision&&who===identity())this.data=parse(value);
        }catch(e){if(rev===revision&&who===identity())this.error="更新公告读取失败："+e.message;}
        finally{if(rev===revision&&who===identity())this.loading=false;}
      },
      close(){
        revision++;this.loading=false;
        if(dialog()?.open)dialog().close();
      },
      onClosed(){
        revision++;this.loading=false;
        if(returnFocus?.isConnected)returnFocus.focus();
        returnFocus=null;
      },
      reset(){shownOnEntry=false;this.close();this.data=empty();this.error="";}
    });
    return vm;
  }
  window.RPAnnouncements={createVM,parse};
})();
