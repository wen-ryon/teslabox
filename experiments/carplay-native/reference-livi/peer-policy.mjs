import net from 'node:net';

const ipv4 = value => {
  if (net.isIP(value) !== 4) throw new Error('control_peer_policy_invalid');
  return value.split('.').reduce((result, part) => (result * 256 + Number(part)) >>> 0, 0);
};
const privateAddress = value => {
  const [a, b] = value.split('.').map(Number);
  return a === 10 || a === 127 || a === 172 && b >= 16 && b <= 31 || a === 192 && b === 168;
};

/** 自动地址只限定握手来源子网；手机身份必须另行验签并匹配，不能把DHCP地址当成身份。 */
export function createPeerPolicy({ host, peer, peerNetwork, authorizeController }) {
  ipv4(host);
  if (!privateAddress(host)) throw new Error('control_peer_policy_invalid');
  if (peerNetwork === undefined) {
    ipv4(peer);
    if (!privateAddress(peer)) throw new Error('control_peer_policy_invalid');
    return { expected: peer, dynamic: false, allows: remote => remote === peer };
  }
  if (peer !== undefined || typeof peerNetwork !== 'string' || typeof authorizeController !== 'function') {
    throw new Error('control_peer_policy_invalid');
  }
  const parts = peerNetwork.split('/');
  if (parts.length !== 2 || !/^(?:1[6-9]|2[0-9]|30)$/.test(parts[1])) throw new Error('control_peer_policy_invalid');
  const prefix = Number(parts[1]), network = ipv4(parts[0]), mask = (0xffffffff << (32 - prefix)) >>> 0;
  const broadcast = (network | (~mask >>> 0)) >>> 0;
  if (!privateAddress(parts[0]) || (network & mask) >>> 0 !== network ||
      (ipv4(host) & mask) >>> 0 !== network || ipv4(host) === network || ipv4(host) === broadcast) {
    throw new Error('control_peer_policy_invalid');
  }
  return { expected: peerNetwork, dynamic: true, allows(remote) {
    if (net.isIP(remote) !== 4 || remote === host) return false;
    const address = ipv4(remote);
    return ((address & mask) >>> 0) === network && address !== network && address !== broadcast;
  } };
}
