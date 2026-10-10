import { EventEmitter } from 'node:events';

// 此实验没有 GStreamer 音频设备；任何误入的媒体路径都必须失败，不能空返回成功。
const unavailable = () => { throw new Error('reference_native_backend_unavailable'); };
const audioEvents = new EventEmitter();
export const DEBUG = false;
export const CP_BT_SOCK_PATH = '';
export const VIDEO_PLANE_MAIN = 0;
export const VIDEO_PLANE_CLUSTER_RECV = 1;
export const openScreenReceiver = unavailable;
export const closeScreenReceiver = unavailable;
export const openAudioReceiver = unavailable;
export const closeAudioReceiver = unavailable;
export const openMicUplink = unavailable;
export const closeMicUplink = unavailable;
export const setAudioReceiverActive = unavailable;
export const setAudioReceiverVolume = unavailable;
/** 仅保留上游构造器的通知注册；没有音频设备，永不伪造 started 事件。 */
export function onAudioReceiverStarted(callback) { audioEvents.on('started', callback); }
/** 独立接收端只允许一条控制会话；退场时释放本次构造器注册，避免反复配对累积闭包。 */
export function clearNativeObservers() { audioEvents.removeAllListeners('started'); }
export const gstHost = new Proxy({ onAudioStarted: onAudioReceiverStarted }, {
  get(target, name) { return target[name] ?? unavailable; }
});
export const AudioCommand = Object.freeze({ AudioPhonecallStart: 4, AudioPhonecallStop: 5,
  AudioNaviStart: 6, AudioNaviStop: 7, AudioVoiceAssistantStart: 8, AudioVoiceAssistantStop: 9,
  AudioMediaStart: 10, AudioMediaStop: 11 });
export const referenceConsole = Object.freeze({ log() {}, warn() {} });
