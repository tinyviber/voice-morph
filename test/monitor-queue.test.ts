import { describe, expect, test } from "bun:test";
import { ChunkSender, STALE } from "../src/monitor-queue";

const block = (v: number) => new Float32Array([v]);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const tick = () => sleep(0); // let the pump's microtask chain reach its next await

interface Sent {
  streamID: number;
  seq: number;
  block: number;
  resolve: (v: string | typeof STALE) => void;
}

/** a send() that parks every call until the test resolves it by hand */
function gatedSend(live: () => boolean = () => true, streamBase = 3000) {
  const calls: Sent[] = [];
  const outputs: string[] = [];
  const sender = new ChunkSender(
    {
      isLive: live,
      send: (streamID, seq, b) => {
        const c: Sent = { streamID, seq, block: b[0]!, resolve: () => {} };
        calls.push(c);
        return new Promise<string | typeof STALE>((r) => (c.resolve = r));
      },
      onOutput: (o) => outputs.push(o),
    },
    3,
    30,
    streamBase,
  );
  return { sender, calls, outputs };
}

describe("ChunkSender", () => {
  test("over the bound it drops the OLDEST unsent block, leaving a seq gap", async () => {
    const { sender, calls, outputs } = gatedSend();
    sender.push(block(1)); // first call is in-flight immediately: (base, seq 0)
    for (const v of [2, 3, 4, 5]) sender.push(block(v)); // cap 3: 1 in flight + 2 queued

    expect(sender.droppedBlocks).toBe(2); // seqs 1 and 2, the stale end, went away

    calls[0]!.resolve("a");
    await tick();
    calls[1]!.resolve("b");
    await tick();
    calls[2]!.resolve("c");
    await tick();

    // the drop leaves a hole in seq — the Go side counts it and runs over it
    expect(calls.map((c) => c.seq)).toEqual([0, 3, 4]);
    expect(calls.map((c) => c.streamID)).toEqual([3000, 3000, 3000]);
    expect(calls.map((c) => c.block)).toEqual([1, 4, 5]); // freshest survive
    expect(outputs).toEqual(["a", "b", "c"]);
    expect(sender.backlog).toBe(0);
  });

  test("IPC timeout resyncs: the next call rides a fresh stream from seq 0", async () => {
    let resolveFirst!: (v: string) => void;
    const sent: Omit<Sent, "resolve">[] = [];
    const outputs: string[] = [];
    const sender = new ChunkSender(
      {
        isLive: () => true,
        send: (streamID, seq, b) => {
          sent.push({ streamID, seq, block: b[0]! });
          // first call answers late (past the 30ms timeout), others are quick
          return sent.length === 1
            ? new Promise<string>((r) => (resolveFirst = r))
            : Promise.resolve(`out${sent.length}`);
        },
        onOutput: (o) => outputs.push(o),
      },
      8,
      30,
      5000,
    );

    sender.push(block(1)); // (5000, 0) — this call will time out
    sender.push(block(2)); // queued behind it
    sender.push(block(3)); // newest queued block survives the resync
    await sleep(80);

    expect(sender.ipcTimeouts).toBe(1);
    expect(sender.resyncs).toBe(1);
    // the timed-out stream is dead: call 2 carries the NEWEST pending
    // block on stream base+1 at seq 0 — block 2's audio is dropped with
    // the old stream (already ≥ a timeout behind reality)
    expect(sent[1]).toEqual({ streamID: 5001, seq: 0, block: 3 });
    expect(sent).toHaveLength(2);
    expect(outputs).toEqual(["out2"]);
    // block 1's abandoned reply will never play, and block 2 was
    // flushed by the resync — two captured blocks never played.
    expect(sender.droppedBlocks).toBe(2);

    // the timed-out call resolving late mutates nothing further client-side
    resolveFirst("late");
    await tick();
    expect(outputs).toEqual(["out2"]);
    expect(sent).toHaveLength(2);

    // and the resynced stream keeps numbering from there
    sender.push(block(9));
    await tick();
    expect(sent[2]).toEqual({ streamID: 5001, seq: 1, block: 9 });
  });

  test("a Go-side stale rejection counts the block and resyncs the stream", async () => {
    const { sender, calls } = gatedSend();
    sender.push(block(1)); // (3000, 0) in flight
    sender.push(block(2)); // queued

    calls[0]!.resolve(STALE); // Go refused our (stream, seq)
    await tick();

    expect(sender.droppedBlocks).toBe(1);
    expect(sender.resyncs).toBe(1);
    // the surviving pending block leaves on the fresh stream, renumbered
    expect(calls[1]).toMatchObject({ streamID: 3001, seq: 0, block: 2 });
  });

  test("repeated timeouts keep resyncing onto fresh stream IDs", async () => {
    const sent: Omit<Sent, "resolve">[] = [];
    const sender = new ChunkSender(
      {
        isLive: () => true,
        send: (streamID, seq, b) => {
          sent.push({ streamID, seq, block: b[0]! });
          return new Promise<string>(() => {}); // never answers
        },
        onOutput: () => {},
      },
      8,
      30,
      7000,
    );

    sender.push(block(1));
    await sleep(50); // times out → resync to 7001
    sender.push(block(2));
    await sleep(50); // times out → resync to 7002
    sender.push(block(3));
    await sleep(50);

    expect(sent.map((s) => s.streamID)).toEqual([7000, 7001, 7002]);
    expect(sent.map((s) => s.seq)).toEqual([0, 0, 0]);
    expect(sender.ipcTimeouts).toBe(3);
    // every timed-out call's in-flight block is also a dropped block —
    // its late reply is discarded, so the audio never plays.
    expect(sender.droppedBlocks).toBe(3);
    expect(sender.resyncs).toBe(3); // the third timeout burns 7003 too (unused)
  });

  test("stop→restart invalidates in-flight replies and freezes the queue", async () => {
    let live = true;
    const { sender, calls, outputs } = gatedSend(() => live);
    sender.push(block(1));
    sender.push(block(2)); // queued behind call 1

    live = false; // monitor stopped while call 1 was in flight
    calls[0]!.resolve("stale");
    await tick();

    expect(outputs).toEqual([]); // stale reply never reached the worklet
    expect(calls).toHaveLength(1); // pump exited; 2 was never sent

    sender.push(block(3)); // a dead generation rejects new blocks outright
    await tick();
    expect(calls).toHaveLength(1);
    expect(sender.backlog).toBe(1); // only 2 remains queued, unsent; 3 never queued
  });

  test("a rejected call loses the block but not the queue or the stream", async () => {
    let n = 0;
    const sent: Omit<Sent, "resolve">[] = [];
    const outputs: string[] = [];
    const sender = new ChunkSender(
      {
        isLive: () => true,
        send: (streamID, seq, b) => {
          sent.push({ streamID, seq, block: b[0]! });
          return ++n === 1 ? Promise.reject(new Error("ipc down")) : Promise.resolve("ok");
        },
        onOutput: (o) => outputs.push(o),
      },
      8,
      30,
      9000,
    );
    sender.push(block(1));
    sender.push(block(2));
    await sleep(20);

    // a transport failure is not a protocol event: same stream, seqs continue
    expect(sent.map((s) => s.streamID)).toEqual([9000, 9000]);
    expect(sent.map((s) => s.seq)).toEqual([0, 1]);
    expect(sender.droppedBlocks).toBe(1); // the failed call's block is counted lost
    expect(sender.ipcTimeouts).toBe(0); // a transport failure is not a timeout
    expect(sender.resyncs).toBe(0);
    expect(outputs).toEqual(["ok"]);
  });
});
