import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { setTimeout as delay } from 'node:timers/promises';

// 等待整棵会话自行回收后才重启，保留身份文件但不复用旧连接和密钥。
export async function superviseReference({ file, args, durationMs, signal, output,
  retryDelayMs = 1000, killGraceMs = 5000, daemon = false }) {
  if (daemon ? durationMs !== null : !Number.isInteger(durationMs) || durationMs < 1) {
    throw new Error('reference_supervisor_duration_invalid');
  }
  const deadline = daemon ? Infinity : performance.now() + durationMs;
  let failures = 0, cycles = 0;
  while (!signal.aborted && performance.now() < deadline) {
    const remaining = daemon ? null : Math.max(1, Math.ceil((deadline - performance.now()) / 1000));
    const child = spawn(file, args(remaining), { stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
    let ready = false, connected = false, ended = false, forced = false, guard;
    const readers = [child.stdout, child.stderr].map(stream => createInterface({ input: stream }));
    for (const reader of readers) reader.on('line', line => {
      if (line === 'REFERENCE_STAGE control_ready') ready = true;
      if (line === 'REFERENCE_STAGE pair_verified') connected = true;
      if (line === 'REFERENCE_STAGE session_closed') ended = true;
      output(line);
    });
    const terminate = () => {
      child.kill('SIGTERM');
      guard ??= setTimeout(() => { forced = true; child.kill('SIGKILL'); }, killGraceMs);
    };
    signal.addEventListener('abort', terminate, { once: true });
    if (signal.aborted) terminate();
    const timer = daemon ? null : setTimeout(terminate, Math.max(1, deadline - performance.now()));
    const code = await new Promise(resolve => {
      child.once('error', () => resolve(-1));
      child.once('close', resolve);
    });
    clearTimeout(timer); clearTimeout(guard);
    signal.removeEventListener('abort', terminate);
    for (const reader of readers) reader.close();
    cycles++;
    // 强制结束不能证明外部硬件和子进程已释放，禁止盲目启动下一轮。
    if (forced) return { reason: 'cleanup_failed', cycles };
    if (signal.aborted) return { reason: 'stopped', cycles };
    if (performance.now() >= deadline) return { reason: 'expired', cycles };
    if (!ready) return { reason: 'startup_failed', cycles };
    if (code === 0 && ended && connected) failures = 0;
    else if (++failures >= 5) return { reason: 'retry_exhausted', cycles };
    output('REFERENCE_STAGE reconnect_wait');
    try {
      await delay(Math.min(retryDelayMs * 2 ** failures, 30000, Math.max(1, deadline - performance.now())), undefined, { signal });
    } catch { return { reason: 'stopped', cycles }; }
  }
  return { reason: signal.aborted ? 'stopped' : 'expired', cycles };
}
