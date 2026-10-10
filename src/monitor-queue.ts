// Pending-block queue + serialized sender for the live monitor.
// Pure policy — no DOM, no bridge — so `bun test` can drive it directly.
//
// Chunk protocol (enforced by Morpher.ProcessChunk in main.go): every
// block travels with (streamID, seq). streamID = the stream base the
// monitor epoch got from ResetStream + the sender's resync index; seq
// numbers every block ADMITTED to the stream, so a block dropped unsent
// leaves a seq hole the Go side counts and processes over. The Go side
// runs each call on its own goroutine and rejects stale (stream, seq)
// pairs before touching DSP state — so when an IPC timeout abandons a
// wait whose call may still be executing, the sender does NOT keep
// feeding that stream: it resyncs onto a fresh stream ID (Go adopts it
// and rebuilds stream state) and resumes from the newest pending block.

export const BLOCK_MS = 2048 / 48; // ≈42.7 ms per captured block at 48 kHz

const TIMEOUT = Symbol("ipc-timeout");
const FAILED = Symbol("ipc-failed");
/** send() resolves with this when Go rejected the (streamID, seq) pair as stale */
export const STALE = Symbol("stale-chunk");

/**
 * Resolve `p`, but no later than `ms`: on timeout resolve TIMEOUT and let
 * `p` finish unseen — its late reply is discarded here and never reaches
 * the worklet. A rejection resolves FAILED. Both mean "this block's audio
 * is gone"; the queue keeps draining instead of stalling on one bad call.
 */
export function callWithTimeout<T>(
  p: Promise<T>,
  ms: number,
): Promise<T | typeof TIMEOUT | typeof FAILED> {
  return new Promise((resolve) => {
    const t = setTimeout(() => resolve(TIMEOUT), ms);
    p.then(
      (v) => {
        clearTimeout(t);
        resolve(v);
      },
      () => {
        clearTimeout(t);
        resolve(FAILED);
      },
    );
  });
}

export interface SenderHooks {
  /**
   * One serialized processChunk IPC round trip. Resolve STALE when the Go
   * side rejected the (streamID, seq) pair as dead — the sender resyncs
   * onto a fresh stream; any other rejection keeps the stream alive.
   */
  send(streamID: number, seq: number, block: Float32Array): Promise<string | typeof STALE>;
  /** a reply that is still fresh enough to play */
  onOutput(outB64: string): void;
  /** false once the owning monitor has stopped (the monitorGen guard) */
  isLive(): boolean;
  /** droppedBlocks / ipcTimeouts / resyncs changed — refresh the stats line */
  onCounts?(): void;
}

export class ChunkSender {
  private pending: { block: Float32Array; seq: number }[] = [];
  private inFlight = 0; // calls sent, awaiting reply (≤1: drain is serial)
  private pumping = false;
  private streamID: number;
  private nextSeq = 0; // blocks admitted on the current stream
  droppedBlocks = 0; // captured blocks discarded without being played
  ipcTimeouts = 0; // processChunk calls that outlived the IPC timeout
  resyncs = 0; // stream IDs this sender burned after timeouts / stale rejections

  constructor(
    private hooks: SenderHooks,
    private cap = 8,
    private timeoutMs = 1800,
    private streamBase = 0, // epoch base from ResetStream; streams are base+resync
  ) {
    this.streamID = streamBase;
  }

  /** queued + in-flight blocks — the send-side latency the queueMs stat estimates */
  get backlog(): number {
    return this.pending.length + this.inFlight;
  }

  get queueMs(): number {
    return Math.round(this.backlog * BLOCK_MS);
  }

  push(block: Float32Array): void {
    if (!this.hooks.isLive()) return; // dead generation: don't even queue
    // Number at admission: a block dropped unsent leaves a seq hole,
    // which the Go side counts and runs over — same content loss as
    // before, now visible in the protocol.
    this.pending.push({ block, seq: this.nextSeq++ });
    // Over the bound the OLDEST unsent block goes — when the engine falls
    // behind, stale audio is discarded so fresh speech gets through first.
    // The worklet's ring cap remains the last-resort bound on the play side.
    let dropped = 0;
    while (this.backlog > this.cap && this.pending.length > 0) {
      this.pending.shift();
      dropped++;
    }
    if (dropped > 0) {
      this.droppedBlocks += dropped;
      this.counted();
    }
    void this.pump();
  }

  /**
   * The current stream is dead: a timed-out call may still be executing
   * under it Go-side, and its tail may have consumed audio this queue no
   * longer has. Move to the next stream ID in the epoch (Go adopts a
   * higher ID and rebuilds stream state) and restart numbering from the
   * NEWEST pending block — everything older is already at least an IPC
   * timeout behind reality, so it is dropped rather than replayed.
   * (Epochs are spaced 1<<20 stream IDs apart on the Go side; a monitor
   * session cannot resync its way into the next epoch's range, and a
   * restart kills the sender anyway.)
   */
  private resync(): void {
    this.resyncs++;
    this.streamID = this.streamBase + this.resyncs;
    const fresh = this.pending.pop();
    const dropped = this.pending.length;
    this.pending.length = 0;
    this.nextSeq = 0;
    if (fresh) {
      fresh.seq = this.nextSeq++;
      this.pending.push(fresh);
    }
    if (dropped > 0) {
      this.droppedBlocks += dropped;
      this.counted();
    }
  }

  /** stats refresh is best-effort — a throwing hook must never kill the pump */
  private counted(): void {
    try {
      this.hooks.onCounts?.();
    } catch {
      /* ignore */
    }
  }

  private async pump(): Promise<void> {
    if (this.pumping) return; // a running loop already picks the block up
    this.pumping = true;
    try {
      for (;;) {
        if (!this.hooks.isLive()) return;
        const item = this.pending.shift();
        if (!item) return;
        this.inFlight++;
        try {
          const out = await callWithTimeout(
            this.hooks.send(this.streamID, item.seq, item.block),
            this.timeoutMs,
          );
          if (out === TIMEOUT) {
            this.ipcTimeouts++;
            this.counted();
            this.resync();
          } else if (out === STALE) {
            // Go refused the (stream, seq): this stream is dead there —
            // count the block lost and move to a fresh stream ID.
            this.droppedBlocks++;
            this.counted();
            this.resync();
          } else if (out === FAILED) {
            this.droppedBlocks++;
            this.counted();
          } else if (this.hooks.isLive()) {
            this.hooks.onOutput(out);
          }
        } catch {
          // a sync send throw or a dead port: the block is lost, drain on
          this.droppedBlocks++;
          this.counted();
        } finally {
          this.inFlight--;
        }
      }
    } finally {
      this.pumping = false;
    }
  }
}

// ── monitor session lifecycle ──────────────────────────────────
// startMonitor used to write the global ctx/stream/node as each await
// resolved and set `running` only after the whole chain finished, so a
// double click ran two initializations whose globals overwrote each
// other — and one attempt's failure could stopMonitor() the OTHER
// attempt's live devices. The supervisor keeps every attempt's
// resources LOCAL until the full chain succeeded AND the generation is
// still current; only then is it committed as the active session.

/**
 * The devices one monitor session owns, filled incrementally by
 * `setup`: assign each field the moment it is acquired so a mid-chain
 * failure releases exactly what exists. Structural types keep this
 * module free of DOM types — the app's real AudioContext / MediaStream /
 * AudioWorkletNode all satisfy them.
 */
export interface MonitorResources {
  ctx?: { close(): unknown };
  stream?: { getTracks(): { stop(): void }[] };
  node?: { disconnect(): void };
  sender?: ChunkSender;
}

export interface MonitorStartDeps {
  /**
   * ResetStream IPC → the stream base the new epoch sends under. Runs
   * BEFORE any audio flows: the Go side rebuilds stream state on the new
   * epoch and rejects every chunk still in flight under an older stream
   * ID, so a dead call can never mutate the new stream.
   */
  resetStream(): Promise<number>;
  /**
   * The full async audio chain (context → worklet → mic → graph →
   * sender). Assign each resource into `res` as it is acquired; on any
   * throw the supervisor releases only what `res` already holds.
   * `isLive` is this attempt's generation check — feed it to the
   * sender's isLive hook.
   */
  setup(res: MonitorResources, streamBase: number, isLive: () => boolean): Promise<void>;
}

/** Tear down a partial or committed session — never touches shared state. */
function releaseMonitor(res: MonitorResources): void {
  try {
    res.node?.disconnect();
  } catch {
    /* ignore */
  }
  try {
    res.stream?.getTracks().forEach((t) => t.stop());
  } catch {
    /* ignore */
  }
  try {
    void res.ctx?.close();
  } catch {
    /* ignore */
  }
}

export class MonitorSupervisor {
  /**
   * Bumped on every start attempt and every stop: the sender's isLive
   * closure dies with it, so queued + in-flight IPC from a dead
   * generation can never land on a newer session's stream.
   */
  gen = 0;
  /** a start's whole async chain holds this — a second click is a no-op */
  private starting = false;
  /** the committed session, present only between start success and stop */
  session: MonitorResources | undefined;

  get running(): boolean {
    return this.session !== undefined;
  }

  /**
   * One guarded start:
   *  - "busy"    — a start is already in flight (double click) or a
   *    session is running; nothing was created.
   *  - "started" — the chain finished while still current and was
   *    committed as the live session.
   *  - "stale"   — stop() raced in mid-setup: this attempt's resources
   *    were released before ever going live; nothing was committed.
   * A setup failure releases only THIS attempt's own resources and
   * rethrows — it cannot tear down another session's devices.
   */
  async start(deps: MonitorStartDeps): Promise<"busy" | "started" | "stale"> {
    if (this.starting || this.session) return "busy";
    this.starting = true;
    const gen = ++this.gen;
    const res: MonitorResources = {};
    try {
      const streamBase = await deps.resetStream();
      if (gen !== this.gen) return "stale"; // stop() raced the IPC
      await deps.setup(res, streamBase, () => gen === this.gen);
      if (gen !== this.gen) {
        // Superseded while setting up: release what THIS attempt
        // acquired — commit nothing, touch nothing else.
        releaseMonitor(res);
        return "stale";
      }
      this.session = res;
      return "started";
    } catch (err) {
      // Kill the failed attempt's own sender (it may have been wired)
      // and release only its own resources — never another session's.
      this.gen++;
      releaseMonitor(res);
      throw err;
    } finally {
      this.starting = false;
    }
  }

  /**
   * Stop the live session (if any) and supersede any start still in
   * flight — a superseded attempt releases its own resources when its
   * chain unwinds, so stop() never has to wait on it.
   */
  stop(): void {
    this.gen++;
    const s = this.session;
    this.session = undefined;
    if (s) releaseMonitor(s);
  }
}
