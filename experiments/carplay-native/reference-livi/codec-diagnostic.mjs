import fs from 'node:fs';
import path from 'node:path';

export const CODEC_DIAGNOSTIC_FILE = 'reference-codec.bin';

/** 诊断只能写进独立会话目录，避免污染下一次启动须逐成员验签的程序树。 */
export function writeCodecDiagnostic(bytes, { directory, runtimeDirectory, allowInsecureTestPlatform = false } = {}) {
  const unix = process.platform === 'linux';
  if (!unix && !allowInsecureTestPlatform) throw new Error('codec_diagnostic_platform_unverified');
  if (!Buffer.isBuffer(bytes) || !bytes.length || bytes.length > 65536) throw new Error('codec_diagnostic_budget');
  for (const value of [directory, runtimeDirectory]) {
    if (typeof value !== 'string' || !path.isAbsolute(value) || path.resolve(value) !== value
        || fs.realpathSync(value) !== value) throw new Error('codec_diagnostic_directory_invalid');
  }
  const related = (parent, child) => {
    const relative = path.relative(parent, child);
    return relative === '' || !path.isAbsolute(relative) && relative !== '..' && !relative.startsWith('..' + path.sep);
  };
  if (related(runtimeDirectory, directory) || related(directory, runtimeDirectory)) throw new Error('codec_diagnostic_runtime_overlap');
  // 同 UID 在私有父目录内恶意竞速不在边界内；拒绝可由其他用户改写的祖先与软链接。
  for (let current = directory; ; current = path.dirname(current)) {
    const stat = fs.lstatSync(current);
    if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error('codec_diagnostic_directory_invalid');
    if (unix && (current === directory
      ? stat.uid !== process.geteuid() || (stat.mode & 0o777) !== 0o700
      : (stat.uid !== 0 && stat.uid !== process.geteuid()) || (stat.mode & 0o022) !== 0 && (stat.mode & 0o1000) === 0)) {
      throw new Error('codec_diagnostic_permissions_invalid');
    }
    if (current === path.dirname(current)) break;
  }
  const target = path.join(directory, CODEC_DIAGNOSTIC_FILE);
  const fd = fs.openSync(target, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL
    | (fs.constants.O_NOFOLLOW ?? 0), 0o600);
  try {
    const stat = fs.fstatSync(fd);
    if (!stat.isFile() || stat.nlink !== 1 || unix && (stat.uid !== process.geteuid() || (stat.mode & 0o777) !== 0o600)) {
      throw new Error('codec_diagnostic_file_invalid');
    }
    fs.writeFileSync(fd, bytes);
  } finally { fs.closeSync(fd); }
  return target;
}
