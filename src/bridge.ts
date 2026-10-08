// Bridge between the page and the Go engine. Inside the app every call is
// the generated typed client; opened in a plain browser (no MyGo runtime)
// it falls back to a mock so the layout can be developed anywhere.

import { isMyGo } from "mygo-runtime";
import { Morpher } from "./mygo";
import type { FileResult, Params, Preset, State } from "./mygo";

export interface Bridge {
  getState(): Promise<State>;
  setParams(p: Params): Promise<void>;
  applyPreset(id: string): Promise<Params>;
  processChunk(pcmB64: string): Promise<string>;
  processFile(wavB64: string): Promise<FileResult>;
  resetStream(): Promise<void>;
}

const real: Bridge = {
  getState: () => Morpher.getState(),
  setParams: (p) => Morpher.setParams(p),
  applyPreset: (id) => Morpher.applyPreset(id),
  processChunk: (pcm) => Morpher.processChunk(pcm),
  processFile: (wav) => Morpher.processFile(wav),
  resetStream: () => Morpher.resetStream(),
};

const mockPresets: Preset[] = [
  { id: "normal", name: "Normal", nameZh: "原声", hint: "不处理", params: { pitch: 0, timbre: 0, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() } },
  { id: "low", name: "Low", nameZh: "低沉", hint: "低半档音高", params: { pitch: -0.5, timbre: 0, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() } },
  { id: "giant", name: "Giant", nameZh: "巨人", hint: "又低又宽", params: { pitch: -0.5, timbre: -0.3, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() } },
  { id: "demon", name: "Demon", nameZh: "恶魔", hint: "极低共振峰", params: { pitch: -0.8, timbre: -0.5, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() } },
  { id: "woman", name: "Woman", nameZh: "女声", hint: "偏高偏细", params: { pitch: 0.45, timbre: 0.28, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() } },
  { id: "high", name: "High", nameZh: "高音", hint: "纯音高上移", params: { pitch: 0.8, timbre: 0, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() } },
  { id: "child", name: "Child", nameZh: "小孩", hint: "高音高共振峰", params: { pitch: 0.8, timbre: 0.4, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() } },
  { id: "chipmunk", name: "Chipmunk", nameZh: "花栗鼠", hint: "极高玩梗", params: { pitch: 1, timbre: 0.55, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() } },
];

function zeros(): number[] {
  return new Array(10).fill(0);
}

const mock: Bridge = {
  getState: async () => ({
    params: { pitch: 0, timbre: 0, strength: 1, gain: 1, bypass: false, eqPre: zeros(), eqPost: zeros() },
    presets: mockPresets,
    eqFreqs: [31.25, 62.5, 125, 250, 500, 1000, 2000, 4000, 8000, 16000],
    sampleRate: 48000,
    latencyMs: 107,
    platform: "browser/mock",
  }),
  setParams: async () => {},
  applyPreset: async (id) => mockPresets.find((p) => p.id === id)!.params,
  // dry pass-through in the browser preview
  processChunk: async (pcm) => pcm,
  processFile: async () => ({ wav: "", seconds: 0, inHz: 48000, outHz: 48000, elapsed: 0 }),
  resetStream: async () => {},
};

export const bridge: Bridge = isMyGo() ? real : mock;
export const inApp = isMyGo();
