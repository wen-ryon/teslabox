// 独立实验客户端：默认 v2；显式 v3 只改变版本校验，不自动回退或获取身份。
import net from 'node:net';

/** 错误只暴露固定阶段码，避免把底层端点或认证内容传给外部日志。 */
export class MfiOracleError extends Error {
  constructor(code) {
    super(code);
    this.name = 'MfiOracleError';
  }
}

/** 固定 loopback 目标与总时限，不允许把实体芯片签名服务转成公网代理。 */
export class MfiOracleSigner {
  #host;
  #port;
  #timeout;
  #major;

  constructor({ host = '127.0.0.1', port, timeoutMs = 4000, expectedMajor = 2 }) {
    if (!['127.0.0.1', '::1'].includes(host) || !Number.isInteger(port)
        || port < 1 || port > 65535 || !Number.isInteger(timeoutMs)
        || timeoutMs < 1 || timeoutMs > 5000 || ![2, 3].includes(expectedMajor)) {
      throw new MfiOracleError('mfi_endpoint_invalid');
    }
    this.#host = host;
    this.#port = port;
    this.#timeout = timeoutMs;
    this.#major = expectedMajor;
  }

  #request(operation, payload = Buffer.alloc(0)) {
    return new Promise((resolve, reject) => {
      const socket = new net.Socket();
      let bytes = Buffer.alloc(0);
      let settled = false;
      const finish = (error, result) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        socket.destroy();
        if (error) reject(new MfiOracleError(error));
        else resolve(result);
      };
      // 总时限包括连接与全部半包，不能用每个 data 回调重置的空闲超时。
      const timer = setTimeout(() => finish('mfi_timeout'), this.#timeout);
      socket.on('error', () => finish('mfi_transport'));
      socket.on('end', () => finish('mfi_response_eof'));
      socket.on('close', () => finish('mfi_response_eof'));
      socket.on('data', chunk => {
        if (settled) return;
        if (bytes.length + chunk.length > 2051) return finish('mfi_response_budget');
        bytes = Buffer.concat([bytes, chunk]);
        if (bytes.length < 3) return;
        const length = bytes.readUInt16BE(1);
        if (bytes[0] !== 0 || length < 1 || length > 2048) return finish('mfi_response_invalid');
        if (bytes.length > length + 3) return finish('mfi_response_trailing');
        if (bytes.length === length + 3) finish(null, Buffer.from(bytes.subarray(3)));
      });
      socket.connect(this.#port, this.#host, () => {
        if (settled) return;
        const header = Buffer.alloc(3);
        header[0] = operation;
        header.writeUInt16BE(payload.length, 1);
        socket.write(Buffer.concat([header, payload]));
      });
    });
  }

  /** 版本必须来自实体服务；旧服务拒绝新操作时直接失败，不回退猜测 2.x。 */
  async protocolMajor() {
    const version = await this.#request(3);
    if (version.length !== 2 || version[0] !== this.#major) {
      throw new MfiOracleError('mfi_protocol_unsupported');
    }
    return version[0];
  }

  /** 只在版本已核对后读取有界 DER 证书，不记录或持久化认证材料。 */
  async certificate() {
    await this.protocolMajor();
    const certificate = await this.#request(1);
    if (certificate.length < 128 || certificate[0] !== 0x30) {
      throw new MfiOracleError('mfi_certificate_invalid');
    }
    return certificate;
  }

  /** 按显式版本接收预哈希摘要；调用前复制，避免等待期间被修改。 */
  async sign(digest) {
    if (!Buffer.isBuffer(digest) || digest.length !== (this.#major === 2 ? 20 : 32)) {
      throw new MfiOracleError('mfi_digest_invalid');
    }
    const payload = Buffer.from(digest);
    await this.protocolMajor();
    const signature = await this.#request(2, payload);
    if (signature.length !== (this.#major === 2 ? 128 : 64)) throw new MfiOracleError('mfi_signature_invalid');
    return signature;
  }
}
