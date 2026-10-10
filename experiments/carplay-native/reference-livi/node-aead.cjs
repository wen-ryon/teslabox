// 纯 Node 实验使用系统 OpenSSL 的真实 AEAD，不加载 Electron 专用 native addon。
const { createCipheriv, createDecipheriv } = require('node:crypto');
const MAX_BODY = 4 * 1024 * 1024;

function valid(key, nonce, data, aad) {
  return Buffer.isBuffer(key) && key.length === 32 && Buffer.isBuffer(nonce) && nonce.length === 12
    && Buffer.isBuffer(data) && data.length <= MAX_BODY + 16
    && Buffer.isBuffer(aad) && aad.length <= 65536;
}

/** 返回真实密文和标签；不支持的运行库或参数必须失败，不能回退明文。 */
function seal(key, nonce, plaintext, aad = Buffer.alloc(0)) {
  if (!valid(key, nonce, plaintext, aad) || plaintext.length > MAX_BODY) throw new Error('aead_arguments_invalid');
  const cipher = createCipheriv('chacha20-poly1305', key, nonce, { authTagLength: 16 });
  cipher.setAAD(aad, { plaintextLength: plaintext.length });
  return Buffer.concat([cipher.update(plaintext), cipher.final(), cipher.getAuthTag()]);
}

/** 与上游 addon 的失败约定一致：只有标签通过后才把明文交给调用者。 */
function open(key, nonce, ciphertext, aad = Buffer.alloc(0)) {
  if (!valid(key, nonce, ciphertext, aad) || ciphertext.length < 16) return null;
  let pending;
  try {
    const length = ciphertext.length - 16;
    const decipher = createDecipheriv('chacha20-poly1305', key, nonce, { authTagLength: 16 });
    decipher.setAAD(aad, { plaintextLength: length });
    decipher.setAuthTag(ciphertext.subarray(length));
    pending = decipher.update(ciphertext.subarray(0, length));
    const tail = decipher.final();
    const output = Buffer.concat([pending, tail]);
    pending.fill(0);
    return output;
  } catch {
    // update 可能先产生未验签的临时明文；失败时不能把它返回或留给媒体回调。
    pending?.fill(0);
    return null;
  }
}

module.exports = { open, seal };
