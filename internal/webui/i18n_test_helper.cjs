// 测试使用真实翻译器，不能用恒等函数掩盖英文模板或参数变化。
const fs=require('node:fs'),vm=require('node:vm');
module.exports=function language(language='zh-CN') {
  const context=vm.createContext({document:{documentElement:{dataset:{deviceLanguage:language}},readyState:'loading',addEventListener(){},querySelectorAll(){return []}}});
  context.TeslaBoxEnglish=JSON.parse(fs.readFileSync(__dirname+'/assets/i18n-en.json','utf8'));
  vm.runInContext(fs.readFileSync(__dirname+'/assets/i18n.js','utf8'),context);
  return context.TeslaBoxI18n;
};
