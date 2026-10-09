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
