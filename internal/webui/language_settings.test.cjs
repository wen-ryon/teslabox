const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
const source=fs.readFileSync(__dirname+'/assets/language-settings.js','utf8');
function fixture({confirm=true,result={language:'en'},error=null}={}) {
  const nodes=new Map(),get=id=>{if(!nodes.has(id))nodes.set(id,{value:'',disabled:false,textContent:'',addEventListener(name,callback){this[name]=callback;}});return nodes.get(id);};
  const requests=[],questions=[];let reloads=0;
  const context=vm.createContext({t:require('./i18n_test_helper.cjs')('zh-CN').t,document:{documentElement:{dataset:{deviceLanguage:'zh-CN'}},getElementById:get},TeslaBoxI18n:require('./i18n_test_helper.cjs')('en'),
    askConfirmation:async(...args)=>{questions.push(args);return confirm;},postJSON:async(path,body)=>{requests.push({path,body:JSON.parse(JSON.stringify(body))});if(error)throw error;return result;},window:{location:{reload(){reloads++;}}}});
  vm.runInContext(source,context);const save=()=>get('language-settings-form').submit({preventDefault(){}});
  return {get,save,requests,questions,reloads:()=>reloads};
}

test('语言来自板端，保存只提交语言枚举，不触碰监管域或客户端存储',async()=>{
  const f=fixture();assert.equal(f.get('interface-language').value,'zh-CN');f.get('interface-language').value='en';await f.save();
  assert.deepEqual(f.requests,[{path:'/api/v1/ui/language',body:{language:'en'}}]);assert.equal(f.reloads(),1);
  assert.match(f.questions[0][0],/Change interface language/);assert.equal(f.get('language-settings-save').disabled,false);
});

test('取消、非法语言、设备未确认及请求失败都不刷新页面',async()=>{
  const canceled=fixture({confirm:false});canceled.get('interface-language').value='en';await canceled.save();assert.equal(canceled.requests.length,0);
  const invalid=fixture();invalid.get('interface-language').value='CN';await invalid.save();assert.equal(invalid.requests.length,0);
  const mismatch=fixture({result:{language:'zh-CN'}});mismatch.get('interface-language').value='en';await mismatch.save();assert.equal(mismatch.reloads(),0);assert.match(mismatch.get('language-settings-detail').textContent,/not confirmed/);
  const failed=fixture({error:Error('原始服务端错误')});failed.get('interface-language').value='en';await failed.save();assert.equal(failed.reloads(),0);assert.equal(failed.get('language-settings-detail').textContent,'原始服务端错误');
});
