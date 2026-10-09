// Pending-block queue + serialized sender for the live monitor.
// Pure policy — no DOM, no bridge — so `bun test` can drive it directly.

export const BLOCK_MS = 2048 / 48; // ≈42.7 ms per captured block at 48 kHz

const TIMEOUT = Symbol("ipc-timeout");
const FAILED = Symbol("ipc-failed");

/**
 * Resolve `p`, but no later than `ms`: on timeout resolve TIMEOUT and let
 * `p` finish unseen — its late reply is discarded here and never reaches
 * the worklet. A rejection resolves FAILED. Both mean "this block's audio
 * is gone"; the queue keeps draining instead of stalling on one bad call.
 */
export function callWithTimeout(
  p: Promise<string>,
  ms: number,
): Promise<string | typeof TIMEOUT | typeof FAILED> {
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
  /** one serialized processChunk IPC round trip */
  send(block: Float32Array): Promise<string>;
  /** a reply that is still fresh enough to play */
  onOutput(outB64: string): void;
  /** false once the owning monitor has stopped (the monitorGen guard) */
  isLive(): boolean;
  /** droppedBlocks / ipcTimeouts changed — refresh the stats line */
  onCounts?(): void;
}

export class ChunkSender {
  private pending: Float32Array[] = [];
  private inFlight = 0; // calls sent, awaiting reply (≤1: drain is serial)
  private pumping = false;
  droppedBlocks = 0; // captured blocks discarded without being played
  ipcTimeouts = 0; // processChunk calls that outlived the IPC timeout

  constructor(
    private hooks: SenderHooks,
    private cap = 8,
    private timeoutMs = 1800,
  ) {}

  /** queued + in-flight blocks — the send-side latency the queueMs stat estimates */
  get backlog(): number {
    return this.pending.length + this.inFlight;
  }

  get queueMs(): number {
    return Math.round(this.backlog * BLOCK_MS);
  }

  push(block: Float32Array): void {
    if (!this.hooks.isLive()) return; // dead generation: don't even queue
    this.pending.push(block);
    // Over the bound the OLDEST unsent block goes — when the engine falls
    // behind, stale audio is discarded so fresh speech gets through first.
    // Dropping an unsent block punches a content discontinuity into the
    // engine's input stream (it never sees that audio): acceptable, and
    // preferable to unbounded latency. The worklet's ring cap remains the
    // last-resort bound on the play side.
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
        const block = this.pending.shift();
        if (!block) return;
        this.inFlight++;
        try {
          const out = await callWithTimeout(this.hooks.send(block), this.timeoutMs);
          if (out === TIMEOUT) {
            this.ipcTimeouts++;
            this.counted();
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
