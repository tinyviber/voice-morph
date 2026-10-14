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
  processChunk(streamID: number, seq: number, pcmB64: string): Promise<string>;
  processFile(wavB64: string): Promise<FileResult>;
  /** issues a new monitor epoch; resolves to that epoch's stream-ID base */
  resetStream(): Promise<number>;
}

const real: Bridge = {
  getState: () => Morpher.getState(),
  setParams: (p) => Morpher.setParams(p),
  applyPreset: (id) => Morpher.applyPreset(id),
  processChunk: (streamID, seq, pcm) => Morpher.processChunk(streamID, seq, pcm),
  processFile: (wav) => Morpher.processFile(wav),
  resetStream: () => Morpher.resetStream(),
};

function mockParams(overrides: Partial<Params> = {}): Params {
  return {
    pitch: 0, timbre: 0, strength: 1, gain: 1, bypass: false,
    speech: false, pitchRange: 1, targetF0: 0, formantShape: 0,
    eqPre: zeros(), eqPost: zeros(),
    ...overrides,
  };
}

const mockPresets: Preset[] = [
  { id: "normal", name: "Normal", nameZh: "原声", hint: "不处理", params: mockParams() },
  { id: "low", name: "Low", nameZh: "低沉", hint: "低半档音高", params: mockParams({ pitch: -0.5 }) },
  { id: "giant", name: "Giant", nameZh: "巨人", hint: "又低又宽", params: mockParams({ pitch: -0.5, timbre: -0.3 }) },
  { id: "demon", name: "Demon", nameZh: "恶魔", hint: "极低共振峰", params: mockParams({ pitch: -0.8, timbre: -0.5 }) },
  { id: "woman", name: "Woman", nameZh: "女声", hint: "偏高偏细", params: mockParams({ pitch: 0.45, timbre: 0.28 }) },
  { id: "high", name: "High", nameZh: "高音", hint: "纯音高上移", params: mockParams({ pitch: 0.8 }) },
  { id: "child", name: "Child", nameZh: "小孩", hint: "高音高共振峰", params: mockParams({ pitch: 0.8, timbre: 0.4 }) },
  { id: "chipmunk", name: "Chipmunk", nameZh: "花栗鼠", hint: "极高玩梗", params: mockParams({ pitch: 1, timbre: 0.55 }) },
  { id: "natural-woman", name: "Natural Woman", nameZh: "自然女声", hint: "柔和 · 适合说话", params: mockParams({ speech: true, targetF0: 220, timbre: 0.2, formantShape: 0.35, pitchRange: 1.1 }) },
  { id: "natural-child", name: "Natural Child", nameZh: "自然童声", hint: "明亮 · 适合说话", params: mockParams({ speech: true, targetF0: 280, timbre: 0.35, formantShape: 0.45, pitchRange: 1.15 }) },
];

function zeros(): number[] {
  return new Array(10).fill(0);
}

let mockEpoch = 0; // pretend stream bases, so restarts look real in preview

const mock: Bridge = {
  getState: async () => ({
    params: mockParams(),
    presets: mockPresets,
    eqFreqs: [31.25, 62.5, 125, 250, 500, 1000, 2000, 4000, 8000, 16000],
    sampleRate: 48000,
    latencyMs: 107,
    platform: "browser/mock",
  }),
  setParams: async () => {},
  applyPreset: async (id) => mockPresets.find((p) => p.id === id)!.params,
  // dry pass-through in the browser preview
  processChunk: async (_streamID, _seq, pcm) => pcm,
  processFile: async () => ({ wav: "", seconds: 0, inHz: 48000, outHz: 48000, elapsed: 0 }),
  resetStream: async () => ++mockEpoch,
};

export const bridge: Bridge = isMyGo() ? real : mock;
export const inApp = isMyGo();
