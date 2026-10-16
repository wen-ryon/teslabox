/* 语言只由设备设置决定；只翻译维护者显式声明的文案，不扫描或改写用户数据。 */
(() => {
  "use strict";
  const language = document.documentElement.dataset.deviceLanguage === "en" ? "en" : "zh-CN";
  const en = Object.freeze(globalThis.TeslaBoxEnglish || {});
  const missing = new Set();
  function t(message, ...values) {
    const template = Array.isArray(message) && Object.prototype.hasOwnProperty.call(message,"raw");
    const key = template ? message.map((part,index) => part + (index < values.length ? `{${index}}` : "")).join("") : String(message);
    let result = key;
    if (language === "en") {
      if (Object.prototype.hasOwnProperty.call(en,key)) result = en[key];
      else if (/[\u3400-\u9fff]/.test(key) && missing.size < 256) missing.add(key);
    }
    // 单次替换避免用户文件名或 SSID 中的 {1} 被当成另一轮占位符处理。
    return template ? result.replace(/\{(\d+)\}/g,(whole,index) => Number(index) < values.length ? String(values[Number(index)]) : whole) : result;
  }
  // 只接受不含插值的编译期 HTML；在私有片段中翻译文案，不能遍历已经混入用户数据的页面。
  t.html = (strings,...values) => {
    if (!Array.isArray(strings) || !Object.prototype.hasOwnProperty.call(strings,"raw") || strings.length !== 1 || values.length) throw new TypeError("Static HTML translation cannot contain interpolated data");
    if (language !== "en") return strings[0];
    const template = document.createElement("template");
    template.innerHTML = strings[0];
    const walker = document.createTreeWalker(template.content,4);
    let current;
    while ((current = walker.nextNode())) {
      const key = current.nodeValue.trim();
      if (!key) continue;
      // option 没有显式 value 时，其原始文案本身就是表单值；先固定原值，避免语言改变协议值。
      const parent = current.parentElement;
      if (parent?.tagName === "OPTION" && !parent.hasAttribute("value")) parent.setAttribute("value",parent.textContent.trim());
      const start = current.nodeValue.indexOf(key);
      current.nodeValue = current.nodeValue.slice(0,start) + t(key) + current.nodeValue.slice(start + key.length);
    }
    for (const element of template.content.querySelectorAll("[title],[placeholder],[aria-label]")) {
      for (const name of ["title","placeholder","aria-label"]) if (element.hasAttribute(name)) element.setAttribute(name,t(element.getAttribute(name)));
    }
    return template.innerHTML;
  };
  function bindStatic() {
    for (const element of document.querySelectorAll("[data-i18n]")) {
      const key = element.getAttribute("data-i18n");
      for (const child of element.childNodes) {
        if (child.nodeType === 3 && child.nodeValue.trim() === key) {
          const start = child.nodeValue.indexOf(key);
          child.nodeValue = child.nodeValue.slice(0,start) + t(key) + child.nodeValue.slice(start + key.length);
          break;
        }
      }
    }
    for (const element of document.querySelectorAll("[data-i18n-title]")) element.setAttribute("title",t(element.getAttribute("data-i18n-title")));
    for (const element of document.querySelectorAll("[data-i18n-placeholder]")) element.setAttribute("placeholder",t(element.getAttribute("data-i18n-placeholder")));
    for (const element of document.querySelectorAll("[data-i18n-aria-label]")) element.setAttribute("aria-label",t(element.getAttribute("data-i18n-aria-label")));
  }
  globalThis.TeslaBoxI18n = {t,language,locale:language === "en" ? "en" : "zh-CN",bindStatic,missing:() => [...missing]};
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded",bindStatic,{once:true}); else bindStatic();
})();
