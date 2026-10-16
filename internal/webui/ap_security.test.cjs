const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const source = fs.readFileSync(__dirname + '/assets/app.js', 'utf8');
const html = fs.readFileSync(__dirname + '/assets/index.html', 'utf8');

function setup(ap = {}) {
  const elements = new Map();
  const get = id => {
    if (!elements.has(id)) elements.set(id, {
      value: '', checked: false, textContent: '', children: [],
      replaceChildren() { this.children = []; },
      append(node) { this.children.push(node); },
    });
    return elements.get(id);
  };
  const calls = [];
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,
    $: get, state: {ap}, document: {createElement: () => ({})},
    askConfirmation: async () => true, setSettingsNotice() {}, loadSettings: async () => {},
    postJSON: async (url, payload) => calls.push({url, payload}),
  });
  vm.runInContext(source.slice(source.indexOf('function renderAPSecurity('), source.indexOf('async function repairAPInterface(')), context);
  vm.runInContext(source.slice(source.indexOf('async function saveAPSettings('), source.indexOf('async function loadStorageDevices(')), context);
  return {get, context, calls};
}

test('AP 页面提供认证选择，并区分已保存配置与接口运行报告', () => {
  for (const id of ['ap-security', 'ap-security-detail', 'ap-configured-security', 'ap-active-security']) {
    assert.ok(html.includes(`id="${id}"`), id);
  }
});

test('没有运行报告不能拿已保存的 WPA3 配置冒充已生效', () => {
  const ap = {security_mode: 'wpa3', security_options: [
    {mode: 'wpa2', label: 'WPA2', available: true},
    {mode: 'wpa2-wpa3', label: '混合', available: false, reason: '后端不支持可靠配置'},
    {mode: 'wpa3', label: 'WPA3', available: false, reason: '能力未知，不代表硬件不支持'},
  ]};
  const {get, context} = setup(ap);
  context.renderAPSecurity(ap, 'running');
  assert.match(get('ap-configured-security').textContent, /WPA3/);
  assert.equal(get('ap-active-security').textContent, '未验证');
  assert.equal(get('ap-security').value, '');
  assert.deepEqual(get('ap-security').children.map(item => Boolean(item.disabled)), [false, false, true, true]);
  assert.match(get('ap-security-detail').textContent, /能力未知/);
});

test('既有 PSK 配置不会被猜成 WPA2 或混合认证，缺少新 API 字段时保持兼容', () => {
  const ap = {security: 'wpa-psk'};
  const {get, context} = setup(ap);
  context.renderAPSecurity(ap, 'disabled');
  assert.match(get('ap-configured-security').textContent, /未确认具体版本/);
  assert.equal(get('ap-active-security').textContent, '未运行');
  assert.equal(get('ap-security').children.length, 1);
});

for (const mode of ['', 'wpa2', 'wpa3']) {
  test(`保存认证 ${mode || '保留'} 时密码留空，保留模式不发送认证改动`, async () => {
    const {get, context, calls} = setup();
    get('ap-enabled').checked = true;
    get('ap-ssid').value = 'TeslaBox';
    get('ap-band').value = 'a';
    get('ap-channel').value = '44';
    get('ap-security').value = mode;
    await context.saveAPSettings({preventDefault() {}});
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, '/api/v1/network/ap');
    assert.equal(calls[0].payload.password, '');
    assert.equal(calls[0].payload.security_mode, mode || undefined);
    assert.equal(Object.hasOwn(calls[0].payload, 'security_mode'), Boolean(mode));
  });
}
