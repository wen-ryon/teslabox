// 执行生产页面配置路径，模拟解码器但保留真实画布和确认代次逻辑。
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

test('动态编码配置不替换画布或销毁播放器，确认使用正确代次', () => {
  const classes = new Set(), players = [], sent = [];
  let replacements = 0;
  const canvas = {hidden: true, style: {}, replaceWith() {replacements++;}};
  const elements = {'#screen': {hidden: true, removeAttribute() {}}, '#notice': {}, '#login': {}, '#canvas': canvas, '#compatibility': {}};
  const context = vm.createContext({
    document: {querySelector: selector => elements[selector], body: {classList: {add: name => classes.add(name), remove: name => classes.delete(name)}}, createElement: () => ({})},
    window: {}, VideoDecoder() {}, isSecureContext: true, performance: {now: () => 10000}, setTimeout() {},
    TeslaBoxHostUI: {create: () => ({})},
    TeslaBoxSession: {create(surface, onFrame, onError, env, keyframe, consumed) {
      const player = {surface, onFrame, consumed, closed: false, codecs: [], configure(codec) {if(this.codecs.length) consumed(123);this.codecs.push(codec);}, close() {this.closed = true;}};
      players.push(player); return player;
    }},
  });
  const source = fs.readFileSync(path.join(__dirname, 'assets/carplay.js'), 'utf8');
  vm.runInContext(source.slice(0, source.indexOf('function configureMse(')), context);
  context.testSocket = {readyState: 1, send: value => sent.push(JSON.parse(value))};
  vm.runInContext("let canvasTouch={cancel(){},dispose(){}},videoTouch={cancel(){}};function bindTouch(){return {cancel(){},dispose(){}};}const connection={healthy(){},stop(error){throw error;}};socket=testSocket;configure({format:'annexb',version:1,codec:'avc1.640029',flowControl:1,epoch:1});", context);
  players[0].onFrame({width: 926, height: 720, renderer: 'webgl2', latencyMs: 82.4, drawMs: 1.7});
  vm.runInContext("configure({format:'annexb',version:1,codec:'avc1.640020',flowControl:1,epoch:2});", context);
  assert.equal(players.length, 1); assert.equal(replacements, 0); assert.equal(players[0].closed, false);
  assert.ok(classes.has('playing')); assert.equal(canvas.hidden, false);
  assert.deepEqual(sent.filter(value => value.type === 'videoAck'), [{type: 'videoAck', epoch: 1, timestamp: 123, rendered: 1, decodeMs: 82, drawMs: 2, renderer: 'webgl2'}]);
  assert.ok(JSON.stringify(sent.at(-2)).length <= 256);
  players[0].consumed(456);
  assert.deepEqual(sent.at(-1), {type: 'videoAck', epoch: 2, timestamp: 456});
  vm.runInContext('stopMedia();', context);
  players[0].consumed(789);
  assert.equal(sent.at(-1).timestamp, 456);
});
