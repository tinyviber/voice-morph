// 离线配音 workspace — review-then-render UI over the Dubber bound
// service. Everything here is a thin shell on the same pipeline the
// voice-dub CLI drives: analyze -> timeline review -> render.
import { Dubber } from "./mygo";
import { inApp } from "./bridge";

interface TimelineSeg {
  segment_id: string;
  asr_segment_id?: string;
  line_id?: string;
  role_id?: string;
  script_text?: string;
  asr_raw: string;
  spoken_text: string;
  start_ms: number;
  end_ms: number;
  match_status: string;
  similarity: number;
  review_required?: boolean;
  review_reasons?: string[];
  take_index?: number;
  selected: boolean;
  boundary?: string;
  edited?: boolean;
  reviewed?: boolean;
  discarded?: boolean;
}
interface Timeline {
  version: number;
  audio: string;
  audio_sha256?: string;
  sample_rate: number;
  duration_ms: number;
  script?: string;
  asr_file?: string;
  segments: TimelineSeg[];
  unmatched_lines?: { line_id: string; role_id: string; text: string; reason: string; optional?: boolean }[];
  review_count: number;
}
interface TextFileRead {
  exists: boolean;
  text?: string;
}

const $ = <T extends HTMLElement>(s: string) => document.querySelector<T>(s)!;
const esc = (s: string) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]!);

let tl: Timeline | null = null;
let tlPath = "";
let audioPath = "";
let scriptPath = "";
let voicesPath = "";
let outDir = "";
const audioBufs = new Map<string, AudioBuffer>();
let actx: AudioContext | null = null;
let manifest: any = null;

const STATUS_ZH: Record<string, string> = {
  exact: "一致", modified: "改写", repeated: "重读", unmatched: "未匹配",
  manual: "人工绑定",
};
const REASON_ZH: Record<string, string> = {
  low_similarity: "相似度低", estimated_boundary: "边界估计",
  ambiguous_takes: "多 take 需选", unmatched_audio: "音频无归属",
  low_asr_confidence: "ASR 置信低", needs_role: "未分配角色",
};

async function call<T = any>(p: Promise<string>): Promise<T> {
  const r = JSON.parse(await p);
  if (!r.ok) throw new Error(r.error || "调用失败");
  return r.data;
}

function toast(msg: string, bad = false) {
  const el = $("#dub-toast");
  el.textContent = msg;
  el.classList.toggle("bad", bad);
  el.classList.add("show");
  setTimeout(() => el.classList.remove("show"), 4000);
}

async function ensureAudioCtx() {
  if (!actx) actx = new AudioContext({ sampleRate: 48000 });
  return actx;
}

function b64ToArrayBuffer(b64: string) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes.buffer;
}

async function playRange(path: string, startMs?: number, endMs?: number) {
  try {
    const ctx = await ensureAudioCtx();
    // Buffers are cached per file — swapping between master take and
    // rendered outputs must never replay the wrong audio.
    let buf = audioBufs.get(path);
    if (!buf) {
      const b64 = await Dubber.readFileB64(path);
      if (!b64) throw new Error("读不到音频文件");
      buf = await ctx.decodeAudioData(b64ToArrayBuffer(b64));
      audioBufs.set(path, buf);
    }
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.connect(ctx.destination);
    const s = (startMs ?? 0) / 1000;
    const dur = endMs != null ? (endMs - (startMs ?? 0)) / 1000 : undefined;
    src.start(0, s, dur);
  } catch (e) {
    toast("播放失败: " + (e as Error).message, true);
  }
}

async function refreshModelStatus() {
  try {
    const ms = await call(Dubber.modelStatus());
    const el = $("#dub-model");
    if (ms.state === "missing") {
      el.textContent = "模型未找到 — 点右侧按钮选择目录";
      el.classList.add("warn");
    } else {
      el.textContent = `模型 ${ms.model_dir}（来源 ${ms.source}${ms.vad_dir ? " +VAD" : ""}）`;
      el.classList.remove("warn");
    }
  } catch (e) {
    $("#dub-model").textContent = "模型状态查询失败: " + (e as Error).message;
  }
}

function roleOptions(sel: string) {
  const roles = new Set<string>();
  tl?.segments.forEach((s) => s.role_id && roles.add(s.role_id));
  tl?.unmatched_lines?.forEach((l) => roles.add(l.role_id));
  roles.add("narrator"); roles.add("character-a"); roles.add("character-b");
  return [...roles].map((r) =>
    `<option value="${esc(r)}"${r === sel ? " selected" : ""}>${esc(r)}</option>`).join("");
}

function renderTable() {
  const body = $("#dub-seg-body");
  if (!tl) {
    body.innerHTML = `<tr><td colspan="9" class="empty">还没有校对结果 — 选音频+台本后点「转写+校对」</td></tr>`;
    return;
  }
  const unLines = tl.unmatched_lines ?? [];
  body.innerHTML = tl.segments.map((s, i) => {
    const status = STATUS_ZH[s.match_status] ?? s.match_status;
    const reasons = (s.review_reasons ?? []).map((r) => REASON_ZH[r] ?? r).join("、");
    const cls = s.review_required ? "flag" : "";
    const take = s.take_index ? `take ${s.take_index}` : "";
    const disposition = s.match_status === "unmatched"
      ? ` <select class="bind-line" data-i="${i}" title="把这段音频绑到一句漏读台词"><option value="">绑到台词…</option>${unLines.map((l) => `<option value="${esc(l.line_id)}">${esc(l.line_id)} ${esc(l.text.slice(0, 12))}</option>`).join("")}</select> <button class="btn mini discard-seg" data-i="${i}" title="明确丢弃此音频">${s.discarded ? "已丢弃" : "丢弃"}</button>`
      : "";
    return `<tr class="${cls}" data-i="${i}">
      <td class="mono">${i + 1}</td>
      <td><button class="btn mini play" data-i="${i}" title="试听原片段">▶</button></td>
      <td class="mono">${(s.start_ms / 1000).toFixed(2)}–${(s.end_ms / 1000).toFixed(2)}</td>
      <td class="script">${esc(s.script_text || "—")}</td>
      <td><input class="spoken" data-i="${i}" value="${esc(s.spoken_text)}"></td>
      <td><select class="role" data-i="${i}"><option value="">（未分配）</option>${roleOptions(s.role_id ?? "")}</select></td>
      <td>${status} ${take} <span class="sim">${s.similarity.toFixed(2)}</span></td>
      <td class="reasons">${esc(reasons)}${s.review_required ? ` <button class="btn mini ok-seg" data-i="${i}" title="确认此片段已审核无误">确认</button>` : ""}${disposition}</td>
      <td><input type="checkbox" class="sel" data-i="${i}"${s.selected ? " checked" : ""}></td>
    </tr>`;
  }).join("");
  // Recount locally every render — confirm/discard/bind change the
  // count, tl.review_count is only the value from the last reconcile.
  const pending = tl.segments.filter((s) =>
    s.review_required || (s.match_status === "unmatched" && !s.discarded)).length;
  $("#dub-review-count").textContent = pending > 0
    ? `${pending} 段待审核` : "全部就绪";
  // Unmatched script lines get a disposition list of their own: each
  // stays "需要补录" (partial at render) until explicitly marked
  // "允许省略" — closing the review loop without editing JSON.
  const un = tl.unmatched_lines ?? [];
  $("#dub-unmatched").innerHTML = un.length === 0 ? "" :
    `${un.length} 句台本无音频：` + un.map((l, i) =>
      `<button class="chip unl" data-i="${i}" title="点击切换处置">${esc(l.line_id)} ${l.optional ? "允许省略" : "需要补录"}</button>`).join(" ");
  document.querySelectorAll<HTMLElement>("#dub-unmatched .unl").forEach((b) =>
    b.addEventListener("click", () => {
      const l = tl!.unmatched_lines![+b.dataset.i!]!;
      l.optional = !l.optional;
      renderTable();
    }));
  body.querySelectorAll<HTMLElement>(".play").forEach((b) =>
    b.addEventListener("click", () => {
      const s = tl!.segments[+b.dataset.i!]!;
      void playRange(audioPath, s.start_ms, s.end_ms);
    }));
  body.querySelectorAll<HTMLInputElement>(".spoken").forEach((el) =>
    el.addEventListener("change", () => {
      const s = tl!.segments[+el.dataset.i!]!;
      s.spoken_text = el.value;
      s.edited = true;
    }));
  body.querySelectorAll<HTMLSelectElement>(".role").forEach((el) =>
    el.addEventListener("change", () => {
      const s = tl!.segments[+el.dataset.i!]!;
      s.role_id = el.value;
      s.edited = true;
      // 未匹配音频被人工分配角色 → 人工绑定（本身就是审核决定）
      if (s.match_status === "unmatched" && el.value) {
        s.match_status = "manual";
        s.selected = true;
        s.review_required = false;
        s.review_reasons = [];
        s.reviewed = true;
      }
      if (el.value) {
        s.review_reasons = (s.review_reasons ?? []).filter((r) => r !== "needs_role");
        if (!s.review_reasons.length && s.reviewed) s.review_required = false;
      }
      renderTable();
    }));
  body.querySelectorAll<HTMLSelectElement>(".bind-line").forEach((el) =>
    el.addEventListener("change", () => {
      const s = tl!.segments[+el.dataset.i!]!;
      const lid = el.value;
      if (!lid) return;
      const line = (tl!.unmatched_lines ?? []).find((l) => l.line_id === lid);
      if (!line) return;
      // Bind this audio span to the missing line: it renders under the
      // line's role and the line leaves the unmatched_lines list.
      s.line_id = line.line_id;
      s.script_text = line.text;
      s.role_id = line.role_id;
      s.match_status = "manual";
      s.selected = true;
      s.edited = true;
      s.reviewed = true; // the bind IS the human disposition
      s.review_required = false;
      s.review_reasons = [];
      s.discarded = false;
      tl!.unmatched_lines = (tl!.unmatched_lines ?? []).filter((l) => l.line_id !== lid);
      renderTable();
    }));
  body.querySelectorAll<HTMLElement>(".discard-seg").forEach((el) =>
    el.addEventListener("click", () => {
      const s = tl!.segments[+el.dataset.i!]!;
      s.discarded = !s.discarded;
      s.edited = true;
      // A deliberate discard is a disposition — discharge the review item.
      if (s.discarded) {
        s.reviewed = true;
        s.review_required = false;
        s.review_reasons = [];
      }
      renderTable();
    }));
  body.querySelectorAll<HTMLElement>(".ok-seg").forEach((el) =>
    el.addEventListener("click", () => {
      const s = tl!.segments[+el.dataset.i!]!;
      s.reviewed = true; // 显式确认才解除审核门，编辑不等于审核
      s.review_required = false;
      s.review_reasons = [];
      renderTable();
    }));
  body.querySelectorAll<HTMLInputElement>(".sel").forEach((el) =>
    el.addEventListener("change", () => {
      const s = tl!.segments[+el.dataset.i!]!;
      s.selected = el.checked;
      s.edited = true;
      if (el.checked && s.line_id) {
        for (const peer of tl!.segments) {
          if (peer === s || peer.line_id !== s.line_id) continue;
          // A selected take replaces every other segment for that line.
          // Selecting a regular part likewise clears an active take, while
          // leaving sibling parts selected as one reading.
          if ((s.take_index ?? 0) > 0 || (peer.take_index ?? 0) > 0) {
            if (peer.selected) {
              peer.selected = false;
              peer.edited = true;
            }
          }
        }
      }
      renderTable();
    }));
}

function clearVoicesPath(message = "未选择（请检查新会话的声线配置）") {
  voicesPath = "";
  $("#dub-voices").textContent = message;
}

function setOutDir(path: string, resetVoices = true) {
  if (path !== outDir && resetVoices) clearVoicesPath();
  outDir = path;
  $("#dub-out").textContent = path;
}

async function pick(field: "audio" | "script" | "voices", title: string, exts: string) {
  const p = await Dubber.pickFile(title, exts);
  if (!p) return;
  if (field === "audio") {
    if (p !== audioPath) clearVoicesPath();
    audioPath = p; audioBufs.delete(p); $("#dub-audio").textContent = p;
  }
  if (field === "script") { scriptPath = p; $("#dub-script").textContent = p; }
  if (field === "voices") { voicesPath = p; $("#dub-voices").textContent = p; }
}

export function initDubbing() {
  if (!inApp) {
    $("#dub-pane").innerHTML =
      `<div class="card"><div class="card-head"><h3>离线配音</h3></div>
       <p class="empty">此工作区需要在 VoiceMorph 应用内运行（文件访问 + 本地 ASR）。</p></div>`;
    return;
  }
  void refreshModelStatus();
  $("#dub-pick-audio").addEventListener("click", () => void pick("audio", "选择录音 WAV", "wav"));
  $("#dub-pick-script").addEventListener("click", () => void pick("script", "选择台本 JSON", "json"));
  $("#dub-pick-voices").addEventListener("click", () => void pick("voices", "选择 voices.json", "json"));
  $("#dub-pick-out").addEventListener("click", async () => {
    const p = await Dubber.pickDirectory("选择输出目录");
    if (p) setOutDir(p);
  });
  $("#dub-pick-model").addEventListener("click", async () => {
    const p = await Dubber.pickDirectory("选择 SenseVoiceSmall 模型目录");
    if (!p) return;
    try {
      await call(Dubber.setModel(p, ""));
      toast("模型已设置");
    } catch (e) { toast((e as Error).message, true); }
    void refreshModelStatus();
  });
  $("#dub-run").addEventListener("click", async () => {
    if (!audioPath) return toast("先选录音 WAV", true);
    if (!outDir) {
      // The default output path completes the current session setup. A
      // voice file explicitly chosen for this audio remains applicable.
      setOutDir(audioPath.replace(/\.[^.]+$/, "") + "-dub", false);
    }
    const btn = $("#dub-run") as HTMLButtonElement;
    btn.disabled = true; btn.textContent = "转写中…（首次加载模型较慢）";
    try {
      const r = await call(Dubber.analyze(audioPath, scriptPath, outDir, ""));
      tlPath = r.timeline ?? "";
      if (r.timeline_doc) { tl = r.timeline_doc; }
      else if (tlPath) { tl = await call(Dubber.loadTimelineFile(tlPath)); }
      renderTable();
      toast(`完成：${r.segments} 段，${r.review_pending ?? 0} 段待审核`);
    } catch (e) {
      toast("转写失败: " + (e as Error).message, true);
    } finally {
      btn.disabled = false; btn.textContent = "转写 + 校对";
    }
  });
  $("#dub-load-timeline").addEventListener("click", async () => {
    const p = await Dubber.pickFile("打开 timeline.json", "json");
    if (!p) return;
    try {
      tl = await call(Dubber.loadTimelineFile(p));
      tlPath = p;
      audioPath = tl!.audio;
      audioBufs.delete(audioPath);
      $("#dub-audio").textContent = audioPath;
      // Reopen the session this timeline belongs to — rendering into a
      // stale or empty outDir was a real footgun.
      setOutDir(p.replace(/[\\/][^\\/]+$/, ""));
      // Session switch: restore this session's voices.json, else clear
      // — reusing the previous session's voice config silently was a
      // real footgun.
      const v = outDir + "/voices.json";
      clearVoicesPath("正在读取会话声线配置…");
      try {
        const loaded = await call<TextFileRead>(Dubber.loadTextFile(v));
        if (loaded.exists) {
          voicesPath = v;
          $("#dub-voices").textContent = v;
        } else {
          $("#dub-voices").textContent = "未选择（该会话无 voices.json）";
        }
      } catch (e) {
        $("#dub-voices").textContent = "读取失败（请检查 voices.json）";
        toast("读取会话 voices.json 失败: " + (e as Error).message, true);
      }
      renderTable();
    } catch (e) { toast((e as Error).message, true); }
  });
  $("#dub-save-review").addEventListener("click", async () => {
    if (!tl || !tlPath) return toast("没有可保存的校对结果", true);
    const reviewed = tlPath.replace(/timeline\.json$/, "timeline-reviewed.json");
    try {
      await call(Dubber.saveTimelineFile(reviewed, JSON.stringify(tl, null, 2)));
      toast(`已保存 ${reviewed}`);
    } catch (e) { toast((e as Error).message, true); }
  });
  $("#dub-render").addEventListener("click", async () => {
    if (!tl) return toast("先完成转写+校对", true);
    if (!tlPath) return toast("缺少 timeline 路径", true);
    // Persist the (possibly edited) timeline first so render sees it.
    const reviewed = tlPath.replace(/timeline\.json$/, "timeline-reviewed.json");
    try {
      await call(Dubber.saveTimelineFile(reviewed, JSON.stringify(tl, null, 2)));
    } catch (e) { return toast((e as Error).message, true); }
    if (!voicesPath) {
      const candidate = outDir ? outDir + "/voices.json" : "";
      if (!candidate) return toast("先选择 voices.json", true);
      let loaded: TextFileRead;
      try {
        loaded = await call<TextFileRead>(Dubber.loadTextFile(candidate));
      } catch (e) {
        return toast("读取 voices.json 失败: " + (e as Error).message, true);
      }
      if (loaded.exists) {
        voicesPath = candidate;
        $("#dub-voices").textContent = candidate;
      } else {
        let confirmed: boolean;
        try {
          confirmed = await call<boolean>(Dubber.confirm(
            "创建默认声线配置", "此会话尚无 voices.json。是否创建默认声线模板？"));
        } catch (e) {
          return toast("确认创建失败: " + (e as Error).message, true);
        }
        if (!confirmed) return;
        try {
          await call(Dubber.createTextFileIfMissing(candidate, await Dubber.defaultVoices()));
        } catch (e) {
          return toast("创建默认 voices.json 失败: " + (e as Error).message, true);
        }
        voicesPath = candidate;
        $("#dub-voices").textContent = candidate + "（默认模板，可编辑）";
      }
    }
    const btn = $("#dub-render") as HTMLButtonElement;
    btn.disabled = true; btn.textContent = "渲染中…";
    try {
      manifest = await call(Dubber.render(reviewed, voicesPath, outDir + "/rendered"));
      // Re-render overwrote files under the same paths — the decoded
      // buffers cached by playRange would keep serving the old audio.
      const renderPrefix = outDir + "/rendered/";
      for (const k of [...audioBufs.keys()])
        if (k.startsWith(renderPrefix)) audioBufs.delete(k);
      renderManifest();
      const fails = manifest.failures?.length ?? 0;
      const skips = manifest.skipped?.length ?? 0;
      const statusLabel: Record<string, string> = {
        complete: "渲染完成", blocked_review: "有待审核未渲染",
        partial: "部分片段缺失", failed: "渲染部分失败",
      };
      toast(`${statusLabel[manifest.status as string] ?? manifest.status}：`
        + `${manifest.lines?.length ?? 0} 段成功${fails ? `，${fails} 段失败` : ""}`
        + `${skips ? `（${skips} 跳过）` : ""}`,
        manifest.status !== "complete");
    } catch (e) {
      toast("渲染失败: " + (e as Error).message, true);
    } finally {
      btn.disabled = false; btn.textContent = "批量渲染";
    }
  });
  $("#dub-open-out").addEventListener("click", () => {
    if (outDir) void Dubber.openPath(outDir);
  });
}

function renderManifest() {
  const box = $("#dub-manifest");
  if (!manifest) { box.innerHTML = ""; return; }
  const lines = (manifest.lines ?? []) as any[];
  box.innerHTML = `
    <div class="card-head"><h3>渲染结果</h3><span class="meta">${manifest.status}</span></div>
    <table class="seg-table"><thead><tr>
      <th>行</th><th>角色</th><th>试听</th><th>区间(s)</th><th>文件</th>
    </tr></thead><tbody>
      ${lines.map((l) => `<tr>
        <td class="mono">${esc(l.line_id ?? l.segment_id)}</td>
        <td>${esc(l.role_id)}${l.experimental ? ' <span class="sim">实验</span>' : ""}</td>
        <td><button class="btn mini mplay" data-w="${esc(l.wav)}">▶</button></td>
        <td class="mono">${(l.cut_start_ms / 1000).toFixed(2)}–${(l.cut_end_ms / 1000).toFixed(2)}</td>
        <td class="mono">${esc(l.wav)}</td>
      </tr>`).join("")}
    </tbody></table>
    <div class="manifest-foot">
      <button id="dub-play-mix" class="btn">▶ 混音轨</button>
      ${(manifest.skipped ?? []).map((s: any) => `<span class="chip warn">跳过 ${esc(s.id)}: ${esc(s.reason)}</span>`).join("")}
      ${(manifest.failures ?? []).map((f: any) => `<span class="chip bad">失败 ${esc(f.segment_id)}: ${esc(f.error)}</span>`).join("")}
      ${(manifest.warnings ?? []).map((w: string) => `<span class="chip warn">${esc(w)}</span>`).join("")}
    </div>`;
  box.querySelectorAll<HTMLElement>(".mplay").forEach((b) =>
    b.addEventListener("click", () => void playRange(outDir + "/rendered/" + b.dataset.w!)));
  $("#dub-play-mix")?.addEventListener("click", () =>
    void playRange(outDir + "/rendered/" + manifest.mix));
}
