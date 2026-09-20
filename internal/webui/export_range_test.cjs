const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require('node:path').join(__dirname, 'assets/app.js'), 'utf8');
function fixture() {
  const nodes = {};
  const context = vm.createContext({activeSession:{duration_seconds:100000,start_at:'2026-09-07T00:00:00Z'}, $:id=>nodes[id] ||= {value:'',style:{setProperty(){}}},formatDuration:String,exportTimeBounds:()=>({duration:100000})});
  vm.runInContext(source.slice(source.indexOf('function setExportRange('),source.indexOf('function localDateTimeInput(')),context);
  return {context,nodes};
}
test('累计位置支持超过24小时且不依赖拍摄日期',()=>{
  const {context:c,nodes:n}=fixture();
  c.setExportRange(1941.9,1951.9);
  assert.equal(n['export-start-time'].value,'00:32:21.9');
  assert.equal(n['export-end-time'].value,'00:32:31.9');
  assert.equal(c.parseExportPosition('27:00:00.1'),97200.1);
  assert.ok(Number.isNaN(c.parseExportPosition('01:60:00')));
});
test('全部步长生效并且开始位置不会推走结束位置',()=>{
  const {context:c,nodes:n}=fixture();
  for(const step of [1,3,5,10,15,30,60]) {
    c.setExportRange(100,300); c.$('export-adjust-step').value=String(step);
    c.adjustExportRange('start:1'); assert.equal(Number(n['export-range-start'].value),100+step);
    c.adjustExportRange('end:-1'); assert.equal(Number(n['export-range-end'].value),300-step);
  }
  c.setExportRange(100,110); c.adjustExportRange('start:1');
  assert.equal(Number(n['export-range-start'].value),109);
  assert.equal(Number(n['export-range-end'].value),110);
});
