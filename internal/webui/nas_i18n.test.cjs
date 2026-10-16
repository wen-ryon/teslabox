const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
const source=fs.readFileSync(__dirname+'/assets/nas-upload.js','utf8');
function fixture(targets=[]) {
  const nodes=new Map(),get=id=>{if(!nodes.has(id))nodes.set(id,{textContent:'',disabled:false});return nodes.get(id);};
  const elements=Object.fromEntries(['name','protocol','host','port','url','user','password','domain','path','host_key','allow_http'].map(key=>[key,{value:'',checked:false}]));
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,translateUI:require('./i18n_test_helper.cjs')('en').t,targets,selector:{value:targets[0]?.id||''},
    form:{elements,reset(){}},qualityForm:{reset(){},querySelector:get},settings:{querySelector:get},qualityStatus:get('quality-status'),status:get('status'),
    setQuality(){},toggleProtocol(){},qualityDirty:false,readQuality:()=>({mode:'original'}),refresh:async()=>{},
    document:{dispatchEvent(){}},CustomEvent:class {constructor(name,options){this.type=name;this.detail=options.detail;}},requests:[],
  });
  context.request=async(path,options)=>{context.requests.push({path,body:JSON.parse(options.body)});return {id:'saved-preview'};};
  return {context,elements,get};
}

test('新增与已有 NAS 目标不遮蔽翻译函数，用户目标名保持原文',()=>{
  const definition=source.match(/^  function fillTarget\(\).*$/m)?.[0];assert.ok(definition);
  for(const targets of [[],[{id:'saved-preview',name:'已连接-这是用户目标名',protocol:'sftp',has_password:true}]]){
    const f=fixture(targets);vm.runInContext(definition+'\nfillTarget();',f.context);
    assert.equal(f.get('#nas-quality-target').textContent,targets[0]?.name||'New server');
    assert.equal(f.get('status').textContent,targets.length?'Dedicated account credentials saved. Leave the password blank to keep them.':'Configure a dedicated upload account.');
  }
});

test('NAS 保存成功提示不调用上传目标对象，协议与用户文本不翻译',async()=>{
  const definition=source.match(/^  form\.onsubmit=.*$/m)?.[0];assert.ok(definition);
  const f=fixture();f.elements.name.value='中文目标';f.elements.protocol.value='sftp';f.elements.host.value='192.0.2.10';f.elements.port.value='22';
  f.elements.user.value='preview-user';f.elements.path.value='中文目录';f.context.fillTarget=()=>{};
  vm.runInContext(definition,f.context);await f.context.form.onsubmit({preventDefault(){},submitter:{disabled:false}});
  assert.equal(f.context.requests.length,1);const payload=f.context.requests[0].body;
  assert.equal(payload.name,'中文目标');assert.equal(payload.path,'中文目录');assert.equal(payload.protocol,'sftp');assert.equal(payload.port,22);
  assert.equal(f.get('status').textContent,'Server profile saved.');
});
