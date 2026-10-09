import { bridge, inApp } from "./bridge";
import { ChunkSender } from "./monitor-queue";
import type { Params, State } from "./mygo";
const workletUrl = "worklet.js"; // served from public/ beside index.html

const $ = <T extends HTMLElement>(s: string) => document.querySelector<T>(s)!;

// ── state ──────────────────────────────────────────────────────
let state: State;
let params: Params;
let eqStage: "pre" | "post" = "pre";
let sendTimer: number | undefined;

const PRESET_GLYPH: Record<string, string> = {
  normal: "◦", low: "▾", giant: "⬣", demon: "♦",
  woman: "❀", high: "▴", child: "✳", chipmunk: "✦",
};

function scheduleSend() {
  clearTimeout(sendTimer);
  sendTimer = window.setTimeout(() => bridge.setParams({ ...params }), 120);
}

// ── UI construction ────────────────────────────────────────────
async function init() {
  state = await bridge.getState();
  params = state.params;

  $("#platform").textContent = state.platform;
  $("#engine-rate").textContent = `${state.sampleRate / 1000}kHz`;
  $("#latency").textContent = `引擎延迟 ~${Math.round(state.latencyMs)}ms`;

  renderPresets();
  renderEq();
  bindFaders();
  bindMonitor();
  bindFile();
  reflectParams();
  drawScope();
}

function renderPresets() {
  const box = $("#presets");
  box.innerHTML = "";
  for (const p of state.presets) {
    const b = document.createElement("button");
    b.className = "preset";
    b.dataset.id = p.id;
    b.innerHTML = `
      <span class="glyph">${PRESET_GLYPH[p.id] ?? "●"}</span>
      <span class="p-name">${p.nameZh} <small>${p.name}</small></span>
      <span class="p-sub">${p.hint}</span>
      <span class="p-vals">P${fmt(p.params.pitch)} T${fmt(p.params.timbre)}</span>`;
    b.onclick = async () => {
      params = await bridge.applyPreset(p.id);
      reflectParams();
      markPreset(p.id);
    };
    box.appendChild(b);
  }
  markPreset("normal");
}

function markPreset(id: string) {
  document
    .querySelectorAll(".preset")
    .forEach((el) => el.classList.toggle("on", (el as HTMLElement).dataset.id === id));
}

const fmt = (v: number) => (v >= 0 ? "+" : "") + v.toFixed(2);

function bindFaders() {
  const pitch = $<HTMLInputElement>("#pitch");
  const timbre = $<HTMLInputElement>("#timbre");
  const strength = $<HTMLInputElement>("#strength");

  // morph params rebuild the engine's streaming stages (~128ms hole):
  // dragging must only update readouts; commit on release ('change')
  const on = (commit: boolean) => () => {
    params.pitch = +pitch.value;
    params.timbre = +timbre.value;
    params.strength = +strength.value / 100;
    reflectReadouts();
    if (commit) scheduleSend();
  };
  for (const el of [pitch, timbre, strength]) {
    el.addEventListener("input", on(false));
    el.addEventListener("change", on(true));
  }

  $("#reset").onclick = () => {
    params.pitch = params.timbre = 0;
    params.strength = 1;
    reflectParams();
    scheduleSend();
  };

  $("#bypass").addEventListener("click", () => {
    params.bypass = !params.bypass;
    $("#bypass").setAttribute("aria-pressed", String(params.bypass));
    $("#bypass").textContent = params.bypass ? "正在旁通" : "旁通原声";
    scheduleSend();
  });
}

function reflectReadouts() {
  $("#pitch-val").textContent = fmt(params.pitch);
  $("#timbre-val").textContent = fmt(params.timbre);
  $("#strength-val").textContent = `${Math.round(params.strength * 100)}%`;
  $("#pitch-note").textContent = `${fmt(params.pitch * 12)} 半音`;
  for (const [id, v, lo, hi] of [
    ["#pitch", params.pitch, -1, 1],
    ["#timbre", params.timbre, -1, 1],
    ["#strength", params.strength * 100, 0, 100],
  ] as const) {
    const el = $<HTMLInputElement>(id);
    el.style.setProperty("--fill", `${((v - lo) / (hi - lo)) * 100}%`);
    el.value = String(v);
  }
}

function reflectParams() {
  reflectReadouts();
  syncEqInputs();
}

// ── EQ ─────────────────────────────────────────────────────────
function renderEq() {
  const box = $("#eq-bands");
  box.innerHTML = "";
  state.eqFreqs.forEach((f, i) => {
    const band = document.createElement("div");
    band.className = "eq-band";
    band.innerHTML = `
      <span class="gain" id="eq-g${i}">0</span>
      <input type="range" min="-12" max="12" step="0.5" value="0" data-i="${i}" orient="vertical" />
      <span class="freq">${f >= 1000 ? `${f / 1000}k` : f}</span>`;
    box.appendChild(band);
  });
  box.querySelectorAll("input").forEach((el) => {
    el.addEventListener("input", () => {
      const i = +(el as HTMLInputElement).dataset.i!;
      const v = +(el as HTMLInputElement).value;
      (eqStage === "pre" ? params.eqPre : params.eqPost)[i] = v;
      $(`#eq-g${i}`).textContent = v ? v.toFixed(1) : "0";
      (el as HTMLInputElement).classList.toggle("hot", v !== 0);
      scheduleSend();
    });
  });
  $("#eq-stage").querySelectorAll("button").forEach((b) => {
    b.addEventListener("click", () => {
      eqStage = b.dataset.stage as "pre" | "post";
      $("#eq-stage")
        .querySelectorAll("button")
        .forEach((x) => x.classList.toggle("on", x === b));
      syncEqInputs();
    });
  });
  $("#eq-flat").onclick = () => {
    (eqStage === "pre" ? params.eqPre : params.eqPost).fill(0);
    syncEqInputs();
    scheduleSend();
  };
}

function syncEqInputs() {
  const gains = eqStage === "pre" ? params.eqPre : params.eqPost;
  document.querySelectorAll<HTMLInputElement>(".eq-band input").forEach((el) => {
    const i = +el.dataset.i!;
    el.value = String(gains[i]);
    el.classList.toggle("hot", gains[i] !== 0);
    $(`#eq-g${i}`).textContent = gains[i] ? gains[i].toFixed(1) : "0";
  });
}

// ── live monitor ───────────────────────────────────────────────
let ctx: AudioContext | undefined;
let stream: MediaStream | undefined;
let node: AudioWorkletNode | undefined;
let resultUrl: string | undefined;
let running = false;
let outLevel = 0;
const scopeBuf = new Float32Array(4800);

// monitorGen invalidates IPC results still in flight across a
// stop→restart: a stale reply (including a timed-out call resolving
// late) can never land on the new session's node.
let monitorGen = 0;
const MAX_BACKLOG = 8; // queued + in-flight blocks before drop-oldest
const IPC_TIMEOUT_MS = 1800; // ≫ engine latency (~110ms): one hung processChunk must never stall the queue
let rtSender: ChunkSender | undefined;

interface RtStats {
  underruns: number;
  overruns: number;
  bufferedMs: number;
}
let lastStats: RtStats = { underruns: 0, overruns: 0, bufferedMs: 0 };

function showRtStats(s?: RtStats) {
  const sender = rtSender;
  if (!sender) return; // stopped (or strays from a dead generation)
  if (s) lastStats = s;
  const el = $("#rt-stats");
  el.hidden = false;
  // end-to-end queue delay ≈ blocks waiting on the send side + the
  // worklet's play-side ring
  const queueMs = sender.queueMs + lastStats.bufferedMs;
  el.title = "排队≈发送队列+播放缓冲的延迟估计；丢块=积压丢弃或调用失败；超时=IPC 无响应";
  el.textContent = `缓冲 ${lastStats.bufferedMs}ms · 排队 ${queueMs}ms · 欠载 ${lastStats.underruns} · 丢块 ${sender.droppedBlocks} · 溢出 ${lastStats.overruns} · 超时 ${sender.ipcTimeouts}`;
}

async function startMonitor() {
  const gen = ++monitorGen;
  ctx = new AudioContext({ sampleRate: 48000, latencyHint: "interactive" });
  await ctx.audioWorklet.addModule(workletUrl);
  stream = await navigator.mediaDevices.getUserMedia({
    audio: {
      channelCount: 1,
      echoCancellation: false,
      noiseSuppression: false,
      autoGainControl: false,
    },
  });
  const src = ctx.createMediaStreamSource(stream);
  node = new AudioWorkletNode(ctx, "duplex", { outputChannelCount: [1] });
  const thisNode = node;
  lastStats = { underruns: 0, overruns: 0, bufferedMs: 0 };
  // serialize IPC through an explicit pending FIFO drained one call at a
  // time; past the bound the OLDEST unsent block is dropped (fresh speech
  // over stale audio), each call is wrapped in an IPC timeout so a hung
  // processChunk drops its result instead of stalling the queue forever.
  const sender = new ChunkSender(
    {
      isLive: () => gen === monitorGen,
      send: (block) => bridge.processChunk(f32ToB64(block)),
      onOutput: (outB64) => {
        const out = b64ToF32(outB64);
        outLevel = rms(out);
        thisNode.port.postMessage(out, [out.buffer]);
      },
      onCounts: () => showRtStats(),
    },
    MAX_BACKLOG,
    IPC_TIMEOUT_MS,
  );
  rtSender = sender;
  thisNode.port.onmessage = (e) => {
    if (e.data.stats) {
      showRtStats(e.data.stats as RtStats);
      return;
    }
    const block: Float32Array = e.data.in;
    if (!block) return;
    feedScope(block);
    sender.push(block);
    showRtStats();
  };
  src.connect(node);
  node.connect(ctx.destination);
  await bridge.resetStream();
  showRtStats();
}

function stopMonitor() {
  monitorGen++; // kill the sender's isLive(): queued + in-flight work dies
  rtSender = undefined;
  node?.disconnect();
  stream?.getTracks().forEach((t) => t.stop());
  ctx?.close();
  node = undefined;
  stream = undefined;
  ctx = undefined;
  $("#rt-stats").hidden = true;
}

function bindMonitor() {
  $("#monitor").addEventListener("click", async () => {
    if (running) {
      stopMonitor();
      running = false;
      $("#monitor").textContent = "开始监听";
      $("#monitor").classList.remove("live");
      $("#mic-status").textContent = "麦克风未开启";
      return;
    }
    try {
      await startMonitor().catch((err) => {
        stopMonitor(); // release mic/graph if setup died midway
        throw err;
      });
      running = true;
      $("#monitor").textContent = "停止监听";
      $("#monitor").classList.add("live");
      $("#mic-status").textContent = inApp
        ? "监听中 — 说话试试"
        : "浏览器预览：音频原样返回（无 Go 引擎）";
    } catch (err) {
      $("#mic-status").textContent = `麦克风不可用：${(err as Error).message}`;
    }
  });
}

// ── scope ──────────────────────────────────────────────────────
function feedScope(block: Float32Array) {
  scopeBuf.copyWithin(0, block.length);
  scopeBuf.set(block.subarray(0, Math.min(block.length, scopeBuf.length)), scopeBuf.length - Math.min(block.length, scopeBuf.length));
}

function drawScope() {
  const cv = $<HTMLCanvasElement>("#scope");
  const g = cv.getContext("2d")!;
  const draw = () => {
    const w = (cv.width = cv.clientWidth);
    const h = cv.height;
    g.clearRect(0, 0, w, h);
    g.strokeStyle = "#d97757";
    g.lineWidth = 1.4;
    g.beginPath();
    for (let x = 0; x < w; x++) {
      const i = Math.floor((x / w) * scopeBuf.length);
      const y = h / 2 - scopeBuf[i]! * h * 0.46;
      x === 0 ? g.moveTo(x, y) : g.lineTo(x, y);
    }
    g.stroke();
    // meters
    $("#meter-in").style.width = `${Math.min(100, rms(scopeBuf) * 400)}%`;
    $("#meter-out").style.width = `${Math.min(100, outLevel * 400)}%`;
    requestAnimationFrame(draw);
  };
  draw();
}

// ── file morph ─────────────────────────────────────────────────
function bindFile() {
  const dz = $("#dropzone");
  const input = $<HTMLInputElement>("#file-input");
  dz.onclick = () => input.click();
  dz.onkeydown = (e) => e.key === "Enter" && input.click();
  dz.ondragover = (e) => {
    e.preventDefault();
    dz.classList.add("drag");
  };
  dz.ondragleave = () => dz.classList.remove("drag");
  dz.ondrop = (e) => {
    e.preventDefault();
    dz.classList.remove("drag");
    const f = e.dataTransfer?.files[0];
    if (f) void morphFile(f);
  };
  input.onchange = () => {
    const f = input.files?.[0];
    input.value = ""; // allow re-picking the same file
    if (f) void morphFile(f);
  };
}

async function morphFile(file: File) {
  const result = $("#file-result");
  dz_busy(true);
  try {
    const raw = await file.arrayBuffer();
    // decode any audio format with Web Audio, resample to 48 kHz mono WAV
    const ac = new AudioContext();
    const buf = await ac.decodeAudioData(raw);
    void ac.close();
    const off = new OfflineAudioContext(1, Math.ceil(buf.duration * 48000), 48000);
    const s = off.createBufferSource();
    s.buffer = buf;
    s.connect(off.destination);
    s.start();
    const rendered = await off.startRendering();
    const mono = rendered.getChannelData(0);
    const wav = encodeWav16(mono, 48000);
    const out = await bridge.processFile(b64Encode(wav));
    if (!out.wav) throw new Error("引擎未返回音频（浏览器预览模式）");
    const wavBytes = b64ToBytes(out.wav);
    const blob = new Blob([wavBytes.buffer as ArrayBuffer], { type: "audio/wav" });
    resultUrl?.startsWith("blob:") && URL.revokeObjectURL(resultUrl);
    const url = URL.createObjectURL(blob);
    resultUrl = url;
    $<HTMLAudioElement>("#result-player").src = url;
    $<HTMLAnchorElement>("#download").href = url;
    result.hidden = false;
    $("#mic-status").textContent = `已处理 ${out.seconds.toFixed(1)}s（${out.inHz}→${out.outHz} Hz）`;
  } catch (err) {
    $("#mic-status").textContent = `处理失败：${(err as Error).message}`;
  } finally {
    dz_busy(false);
  }
}

function dz_busy(b: boolean) {
  document.querySelector(".file-card")!.classList.toggle("busy", b);
}

// ── codecs ─────────────────────────────────────────────────────
function f32ToB64(x: Float32Array): string {
  const bytes = new Uint8Array(x.buffer, x.byteOffset, x.byteLength);
  return b64Encode(bytes);
}
function b64ToF32(s: string): Float32Array {
  const b = b64ToBytes(s);
  return new Float32Array(b.buffer, b.byteOffset, b.byteLength / 4);
}
function b64Encode(b: Uint8Array): string {
  let s = "";
  for (let i = 0; i < b.length; i += 8192) s += String.fromCharCode(...b.subarray(i, i + 8192));
  return btoa(s);
}
function b64ToBytes(s: string): Uint8Array {
  const bin = atob(s);
  const b = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) b[i] = bin.charCodeAt(i);
  return b;
}
function rms(x: Float32Array): number {
  let e = 0;
  for (const v of x) e += v * v;
  return Math.sqrt(e / x.length);
}
function encodeWav16(samples: Float32Array, sr: number): Uint8Array {
  const n = samples.length;
  const buf = new ArrayBuffer(44 + n * 2);
  const v = new DataView(buf);
  const wstr = (o: number, s: string) => [...s].forEach((c, i) => v.setUint8(o + i, c.charCodeAt(0)));
  wstr(0, "RIFF");
  v.setUint32(4, 36 + n * 2, true);
  wstr(8, "WAVE");
  wstr(12, "fmt ");
  v.setUint32(16, 16, true);
  v.setUint16(20, 1, true);
  v.setUint16(22, 1, true);
  v.setUint32(24, sr, true);
  v.setUint32(28, sr * 2, true);
  v.setUint16(32, 2, true);
  v.setUint16(34, 16, true);
  wstr(36, "data");
  v.setUint32(40, n * 2, true);
  for (let i = 0; i < n; i++) {
    const s = Math.max(-1, Math.min(1, samples[i]!));
    v.setInt16(44 + i * 2, s * 32767, true);
  }
  return new Uint8Array(buf);
}

void init();
