const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const vm = require('node:vm');
const { pathToFileURL } = require('node:url');
const modulePath = path.resolve(__dirname, '../../experiments/carplay-native/reference-livi/codec-diagnostic.mjs');
const runSource = fs.readFileSync(path.join(path.dirname(modulePath), 'run-reference.mjs'), 'utf8');
const moduleReady = import(pathToFileURL(modulePath));

function fixture(t) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'teslabox-codec-test-')));
  const runtimeDirectory = path.join(root, 'version');
  const directory = path.join(root, 'session');
  fs.mkdirSync(runtimeDirectory, { mode: 0o700 });
  fs.mkdirSync(directory, { mode: 0o700 });
  fs.writeFileSync(path.join(runtimeDirectory, 'teslabox-carplay'), 'fixed-native', { mode: 0o755 });
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  return { root, runtimeDirectory, directory, allowInsecureTestPlatform: process.platform !== 'linux' };
}

function actualCallback(options, writer, stages) {
  const match = runSource.match(/onCodecConfig: (bytes => \{[\s\S]*?\n    \}),\n    onObservation:/);
  assert.ok(match, '须执行运行入口的真实 codec 回调');
  return vm.runInNewContext(`(${match[1]})`, {TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t, Buffer, codecSaved: false,
    temporary: options.directory, values: { binary: path.join(options.runtimeDirectory, 'teslabox-carplay') }, path,
    writeCodecDiagnostic: (bytes, args) => writer(bytes, { ...args, allowInsecureTestPlatform: options.allowInsecureTestPlatform }),
    stage: name => stages.push(name) });
}

test('真实首个 codec 回调不修改版本目录，重复回调只写一次，退出清理会话', async t => {
  const { writeCodecDiagnostic, CODEC_DIAGNOSTIC_FILE } = await moduleReady;
  const options = fixture(t), stages = [], before = fs.readdirSync(options.runtimeDirectory);
  const callback = actualCallback(options, writeCodecDiagnostic, stages);
  callback(Buffer.from('codec')); callback(Buffer.from('another-codec'));
  assert.deepEqual(fs.readdirSync(options.runtimeDirectory), before);
  assert.equal(fs.readFileSync(path.join(options.directory, CODEC_DIAGNOSTIC_FILE), 'utf8'), 'codec');
  assert.deepEqual(stages, ['screen_codec_saved']);
  if (process.platform === 'linux') assert.equal(fs.statSync(path.join(options.directory, CODEC_DIAGNOSTIC_FILE)).mode & 0o777, 0o600);
  const cleanup = runSource.match(/  if \(temporary\) \{[\s\S]*?\n  \}\n  stage\('owned_resources_stopped'\)/);
  assert.ok(cleanup, '须执行真实 finally 的诊断清理');
  vm.runInNewContext(cleanup[0], {TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t, temporary: options.directory, fs, path, CODEC_DIAGNOSTIC_FILE, process: {}, stage: () => {} });
  assert.equal(fs.existsSync(options.directory), false);
  assert.deepEqual(fs.readdirSync(options.runtimeDirectory), before);
});

test('诊断路径错误只记录保存失败，不改版本树或抛出终止媒体异常', async t => {
  const { writeCodecDiagnostic } = await moduleReady;
  const options = fixture(t), stages = [];
  options.directory = options.runtimeDirectory;
  actualCallback(options, writeCodecDiagnostic, stages)(Buffer.from('codec'));
  assert.deepEqual(stages, ['screen_codec_save_failed']);
  assert.deepEqual(fs.readdirSync(options.runtimeDirectory), ['teslabox-carplay']);
});

test('拒绝相对路径、版本树重叠、非目录、缺失路径和超限内容', async t => {
  const { writeCodecDiagnostic } = await moduleReady;
  const options = fixture(t);
  const child = path.join(options.runtimeDirectory, 'private'); fs.mkdirSync(child, { mode: 0o700 });
  for (const [name, args, bytes] of [
    ['relative', { ...options, directory: 'relative' }, Buffer.from('codec')],
    ['runtime-equal', { ...options, directory: options.runtimeDirectory }, Buffer.from('codec')],
    ['runtime-child', { ...options, directory: child }, Buffer.from('codec')],
    ['runtime-ancestor', { ...options, directory: options.root }, Buffer.from('codec')],
    ['not-directory', { ...options, directory: path.join(options.runtimeDirectory, 'teslabox-carplay') }, Buffer.from('codec')],
    ['missing', { ...options, directory: path.join(options.root, 'missing') }, Buffer.from('codec')],
    ['empty', options, Buffer.alloc(0)], ['oversized', options, Buffer.alloc(65537)], ['not-buffer', options, 'codec']
  ]) await t.test(name, () => assert.throws(() => writeCodecDiagnostic(bytes, args)));
  assert.deepEqual(fs.readdirSync(options.directory), []);
});

test('已有文件或硬链接不能覆盖', async t => {
  const { writeCodecDiagnostic, CODEC_DIAGNOSTIC_FILE } = await moduleReady;
  const options = fixture(t), original = path.join(options.root, 'original');
  fs.writeFileSync(original, 'keep');
  fs.linkSync(original, path.join(options.directory, CODEC_DIAGNOSTIC_FILE));
  assert.throws(() => writeCodecDiagnostic(Buffer.from('codec'), options));
  assert.equal(fs.readFileSync(original, 'utf8'), 'keep');
});

test('拒绝软链接目录或祖先、宽松权限以及诊断文件软链接', { skip: process.platform !== 'linux' }, async t => {
  const { writeCodecDiagnostic, CODEC_DIAGNOSTIC_FILE } = await moduleReady;
  const options = fixture(t), alias = path.join(options.root, 'alias');
  fs.symlinkSync(options.directory, alias);
  assert.throws(() => writeCodecDiagnostic(Buffer.from('codec'), { ...options, directory: alias }));
  const nested = path.join(options.directory, 'nested'); fs.mkdirSync(nested, { mode: 0o700 });
  assert.throws(() => writeCodecDiagnostic(Buffer.from('codec'), { ...options, directory: path.join(alias, 'nested') }));
  fs.chmodSync(options.directory, 0o755);
  assert.throws(() => writeCodecDiagnostic(Buffer.from('codec'), options));
  fs.chmodSync(options.directory, 0o700);
  const target = path.join(options.root, 'keep'); fs.writeFileSync(target, 'keep');
  fs.symlinkSync(target, path.join(options.directory, CODEC_DIAGNOSTIC_FILE));
  assert.throws(() => writeCodecDiagnostic(Buffer.from('codec'), options));
  assert.equal(fs.readFileSync(target, 'utf8'), 'keep');
});

test('产品入口不放宽非 Linux 平台', { skip: process.platform === 'linux' }, async t => {
  const { writeCodecDiagnostic } = await moduleReady;
  assert.throws(() => writeCodecDiagnostic(Buffer.from('codec'), { ...fixture(t), allowInsecureTestPlatform: false }), /platform_unverified/);
});
