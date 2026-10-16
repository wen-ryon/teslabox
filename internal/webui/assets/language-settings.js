/* 语言来自设备共享设置，不根据当前浏览器缓存或无线地区推断。 */
(() => {
  "use strict";
  const t = globalThis.TeslaBoxI18n.t;
  const form=document.getElementById("language-settings-form");
  const select=document.getElementById("interface-language");
  const button=document.getElementById("language-settings-save");
  const detail=document.getElementById("language-settings-detail");
  if (!form) return;
  select.value=document.documentElement.dataset.deviceLanguage || "zh-CN";
  let busy=false;
  form.addEventListener("submit",async event=>{
    event.preventDefault();
    if (busy) return;
    const language=select.value;
    if (!["zh-CN","en"].includes(language)) return;
    if (!await askConfirmation(t("切换界面语言"),t("语言设置将保存到设备并刷新当前页面，请先保存其他未提交的修改。其他客户端刷新后使用同一语言；不会修改无线地区、系统时区或原始日志。"),t("保存并刷新"),false)) return;
    busy=true; button.disabled=true;
    try {
      const result=await postJSON("/api/v1/ui/language",{language});
      if (result.language!==language) throw new Error(t("设备未确认所选语言，尚未刷新页面"));
      window.location.reload();
    } catch(error) { detail.textContent=error.message; detail.className="form-error"; }
    finally {busy=false;button.disabled=false;}
  });
})();
