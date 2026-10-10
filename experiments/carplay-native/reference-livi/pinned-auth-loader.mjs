// 加载固定 GPL 参考协议模块；修改只限 Node 运行适配和日志脱敏，不复制进正式包。
import { readFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { registerHooks, stripTypeScriptTypes } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import path from 'node:path';

const root = path.resolve(fileURLToPath(new URL('../../../build/livi-reference-a956223/', import.meta.url)));
const stack = path.join(root, 'src/main/services/projection/driver/cp/stack');
const hashes = new Map([
  ['cpStack.ts', '9862d0ce6786a7f2fecf1444cfac9b5a419fe5adbc86aa8061f7a218ab4e871c'],
  ['getInfo.ts', 'a384528b42ef6884dceea68fb2b0ab36112cd5dcf35b929f06eb564173ddddcf'],
  ['hid.ts', 'e54b517b3f4331d05d76a851f47e5e4686e0d0fd42c0b00c89a89e5220bfdfc6'],
  ['timingServer.ts', 'fd6ffbca10fb13cf935434963ff250c8fab2d405b6af52332ea8dd1e572d3b32'],
  ['keepAliveServer.ts', 'b898fb50d558fbca9b90539f7e60815fc1be8f6d9f5a5e936eb8ecd3d1b0d64b'],
  ['ntp.ts', 'a2904de83dee4a2c9f8e40a4771076c8a641a18118cb4d45fac0f0489325122d'],
  ['authSetup.ts', '95f032f4c7d50cbcdc99a86471c311651b7ffc4a0ee11a1514b8762ab6fbb75d'],
  ['crypto.ts', 'a3df125c16b9154e483fae47e079525ef87ffba98f9ff934ff48e6a1ba1635dc'],
  ['screenStream.ts', '1a8e82c2e8d6d3531774eeb107e50d066fd48072f7f2b4b747f0503fcfd83a1c'],
  ['nalu.ts', '16ce00746085dca085fd8916a82c7ad9d37b095719299985ac2d3f7f21bc839f'],
  ['pairVerify.ts', '53698fa466d54a9e0ba2e11562226c08931b25bccced3b7ff44946cb03f0cd58'],
  ['tlv8.ts', 'c3c8153033ca6c48a599ffffd2234345eaf18e01d5dd31e36ac032fbd97acb2f'],
  ['pairSetup.ts', '2282380828cc7dbc728cf5b1953363f9c72d22fdff941e1fdf9857728febb0de'],
  ['srp.ts', 'c39a8bcc64eae04b75f0f497ad40942a891ac8913c0ede3c79353ae8c03de50d'],
  ['iapTunnel.ts', '054d78f7f3b91dfdf13a0532b084893445a66a2bfce40e965e9b6defe739db7a'],
  ['controlCipher.ts', 'd977b32dc2e64d119ddc071d0536870b456576a709560dd8f96137d9c984b642'],
  ['rtspMessage.ts', 'c03db0cbd799f137f1646b538ad27301f432bea4393f2360f6df478f75c9028f'],
  ['bplist.ts', '464adf69795eb38037da422b6b8e6e2e3d363e6ccf9844a9cca6c0d1685dbab4']
]);
const urls = new Map([...hashes].map(([name, hash]) => [pathToFileURL(path.join(stack, name)).href, hash]));

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (urls.has(context.parentURL) && specifier.startsWith('./') && hashes.has(`${specifier.slice(2)}.ts`)) {
      return { url: pathToFileURL(path.join(stack, `${specifier.slice(2)}.ts`)).href, shortCircuit: true };
    }
    if (context.parentURL === pathToFileURL(path.join(stack, 'cpStack.ts')).href &&
        ['@main/constants', '@main/services/video/GstVideo', '@main/services/video/gstHost',
          '@shared/types/ProjectionEnums', '../CpHelperSock'].includes(specifier)) {
      return { url: new URL('./native-boundary.mjs', import.meta.url).href, shortCircuit: true };
    }
    if (['pairVerify.ts', 'pairSetup.ts'].some((name) => context.parentURL === pathToFileURL(path.join(stack, name)).href) &&
        ['./identity', './pairings'].includes(specifier)) {
      return { url: new URL('./identity-binding.mjs', import.meta.url).href, shortCircuit: true };
    }
    if (specifier === 'livi-crypto' && context.parentURL === pathToFileURL(path.join(stack, 'crypto.ts')).href) {
      return { url: new URL('./node-aead.cjs', import.meta.url).href, shortCircuit: true };
    }
    return nextResolve(specifier, context);
  },
  load(url, context, nextLoad) {
    if (!urls.has(url)) return nextLoad(url, context);
    let source = readFileSync(fileURLToPath(url), 'utf8').replaceAll('\r\n', '\n');
    if (createHash('sha256').update(source).digest('hex') !== urls.get(url)) {
      throw new Error('livi_reference_hash_mismatch');
    }
    if (url.endsWith('/crypto.ts')) {
      source = "import { createRequire } from 'node:module';\nconst require = createRequire(import.meta.url);\n" + source;
    }
    if (url.endsWith('/cpStack.ts') || url.endsWith('/timingServer.ts')) {
      // 原日志可能展开手机身份和报文；会话适配器另行记录白名单阶段，不输出原始参数。
      source = "import { referenceConsole as console } from " + JSON.stringify(new URL('./native-boundary.mjs', import.meta.url).href) + ";\n" + source;
    }
    if (url.endsWith('/screenStream.ts')) {
      // 只替换三条含端点或底层异常的日志，分帧和解密源码仍按固定实现执行。
      const edits = [
        ["console.log(`[cpScreen] video data connection from ${sock.remoteAddress}:${sock.remotePort}`)", "console.log('[cpScreen] video data connection accepted')"],
        ["console.warn('[cpScreen] frame error:', (e as Error).message)", "console.warn('[cpScreen] frame rejected')"],
        ["console.warn(`[cpScreen] socket error: ${err.message}`)", "console.warn('[cpScreen] socket error')"]
      ];
      for (const [before, after] of edits) {
        if (source.split(before).length !== 2) throw new Error('livi_reference_edit_mismatch');
        source = source.replace(before, after);
      }
    }
    if (url.endsWith('/pairVerify.ts')) {
      const edits = [
        ["console.warn('[pairVerify] error:', (e as Error).message)", "console.warn('[pairVerify] rejected')"],
        ["console.warn(`[pairVerify] unknown controller ${ctrlId.toString('utf8')}`)", "console.warn('[pairVerify] unknown controller')"],
        ["console.log(`[pairVerify] M3->M4 verified (controller ${this._controllerId})`)", "console.log('[pairVerify] M3->M4 verified')"]
      ];
      for (const [before, after] of edits) {
        if (source.split(before).length !== 2) throw new Error('livi_reference_edit_mismatch');
        source = source.replace(before, after);
      }
    }
    if (url.endsWith('/pairSetup.ts')) {
      const edits = [
        ["console.warn('[pairSetup] error:', (e as Error).message)", "console.warn('[pairSetup] rejected')"],
        ["console.log(`[pairSetup] M5->M6 paired (controller ${ctrlId.toString('utf8')})`)", "console.log('[pairSetup] M5->M6 paired')"]
      ];
      for (const [before, after] of edits) {
        if (source.split(before).length !== 2) throw new Error('livi_reference_edit_mismatch');
        source = source.replace(before, after);
      }
    }
    if (url.endsWith('/iapTunnel.ts')) {
      const edits = [
        ["console.log(`[cpIapTunnel] iAP data connection from ${sock.remoteAddress}:${sock.remotePort}`)", "console.log('[cpIapTunnel] iAP data connection accepted')"],
        ["console.warn(`[cpIapTunnel] socket error: ${e.message}`)", "console.warn('[cpIapTunnel] socket error')"],
        ["console.warn(`[cpIapTunnel] stream decrypt failed: ${(e as Error).message}`)", "console.warn('[cpIapTunnel] stream decrypt rejected')"],
        ["console.warn(`[cpIapTunnel] implausible package size ${size}, dropping`)", "console.warn('[cpIapTunnel] package size rejected')"]
      ];
      for (const [before, after] of edits) {
        if (source.split(before).length !== 2) throw new Error('livi_reference_edit_mismatch');
        source = source.replace(before, after);
      }
    }
    return { format: 'module', source: stripTypeScriptTypes(source, { mode: 'transform' }), shortCircuit: true };
  }
});

/** 直接执行固定上游 auth-setup，不能用自写的响应器冒充参考实现。 */
export async function loadPinnedAuthSetup() {
  return import(pathToFileURL(path.join(stack, 'authSetup.ts')).href);
}

/** 保留上游 ScreenStream 的实际分帧、配置读取与视频 AEAD；安全外围由适配层提供。 */
export async function loadPinnedScreenStream() {
  return import(pathToFileURL(path.join(stack, 'screenStream.ts')).href);
}

/** 执行固定 Pair-Verify；身份持久化单独适配，握手算法保持上游源码。 */
export async function loadPinnedPairVerify() {
  return import(pathToFileURL(path.join(stack, 'pairVerify.ts')).href);
}

/** 执行固定 SRP/Pair-Setup；存储写入仍由单次限时授权控制。 */
export async function loadPinnedPairSetup() {
  return import(pathToFileURL(path.join(stack, 'pairSetup.ts')).href);
}

/** 执行固定 iAP DataStream 解密和包解析；端口、来源及消费者所有权由外围接管。 */
export async function loadPinnedIapTunnel() {
  return import(pathToFileURL(path.join(stack, 'iapTunnel.ts')).href);
}

/** 保留固定事件加密、RTSP 构建及 plist 编码；不调用上游无界的网络入口。 */
export async function loadPinnedEventCodecs() {
  const [cipher, rtsp, plist, crypto] = await Promise.all(['controlCipher.ts', 'rtspMessage.ts', 'bplist.ts', 'crypto.ts']
    .map(name => import(pathToFileURL(path.join(stack, name)).href)));
  return { ...cipher, ...rtsp, ...plist, hkdfSha512: crypto.hkdfSha512 };
}

/** 加载完整固定控制类；原生媒体调用必须由真实适配器接管，边界默认明确报错。 */
export async function loadPinnedStack() {
  const [stackModule, timing, keepAlive, info] = await Promise.all(['cpStack.ts', 'timingServer.ts', 'keepAliveServer.ts', 'getInfo.ts']
    .map(name => import(pathToFileURL(path.join(stack, name)).href)));
  return { ...stackModule, ...timing, ...keepAlive, ...info };
}

/** 媒体指令复用已声明的固定 HID 编号，不能把播放/暂停切换当作播放。 */
export async function loadPinnedHid() {
  return import(pathToFileURL(path.join(stack, 'hid.ts')).href);
}
