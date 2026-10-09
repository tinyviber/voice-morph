import { describe, expect, test } from "bun:test";
import { ChunkSender } from "../src/monitor-queue";

const block = (v: number) => new Float32Array([v]);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const tick = () => sleep(0); // let the pump's microtask chain reach its next await

interface Call {
  block: number;
  resolve: (v: string) => void;
}

/** a send() that parks every call until the test resolves it by hand */
function gatedSend(live: () => boolean = () => true) {
  const calls: Call[] = [];
  const outputs: string[] = [];
  const sender = new ChunkSender(
    {
      isLive: live,
      send: (b) => {
        const c: Call = { block: b[0]!, resolve: () => {} };
        calls.push(c);
        return new Promise<string>((r) => (c.resolve = r));
      },
      onOutput: (o) => outputs.push(o),
    },
    3,
    30,
  );
  return { sender, calls, outputs };
}

describe("ChunkSender", () => {
  test("over the bound it drops the OLDEST unsent block, not the newest", async () => {
    const { sender, calls, outputs } = gatedSend();
    sender.push(block(1)); // first call is in-flight immediately
    for (const v of [2, 3, 4, 5]) sender.push(block(v)); // cap 3: 1 in flight + 2 queued

    expect(sender.droppedBlocks).toBe(2); // 2 and 3, the stale end, went away

    calls[0]!.resolve("a");
    await tick();
    calls[1]!.resolve("b");
    await tick();
    calls[2]!.resolve("c");
    await tick();

    expect(calls.map((c) => c.block)).toEqual([1, 4, 5]); // freshest survive
    expect(outputs).toEqual(["a", "b", "c"]);
    expect(sender.backlog).toBe(0);
  });

  test("IPC timeout drops the call's result and keeps the queue draining", async () => {
    let resolveFirst!: (v: string) => void;
    const sent: number[] = [];
    const outputs: string[] = [];
    const sender = new ChunkSender(
      {
        isLive: () => true,
        send: (b) => {
          sent.push(b[0]!);
          // first call answers late (past the 30ms timeout), second is quick
          return sent.length === 1
            ? new Promise<string>((r) => (resolveFirst = r))
            : Promise.resolve("out2");
        },
        onOutput: (o) => outputs.push(o),
      },
      8,
      30,
    );

    sender.push(block(1));
    sender.push(block(2));
    await sleep(80);

    expect(sender.ipcTimeouts).toBe(1); // call 1 timed out…
    expect(sent).toEqual([1, 2]); // …and the queue moved on to call 2
    expect(outputs).toEqual(["out2"]);

    resolveFirst("late"); // the timed-out call resolving late is dropped unseen
    await tick();
    expect(outputs).toEqual(["out2"]);
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
    expect(calls.map((c) => c.block)).toEqual([1]); // pump exited; 2 was never sent

    sender.push(block(3)); // a dead generation rejects new blocks outright
    await tick();
    expect(calls.map((c) => c.block)).toEqual([1]);
    expect(sender.backlog).toBe(1); // only 2 remains queued, unsent; 3 never queued
  });

  test("a rejected call loses the block but not the queue", async () => {
    let n = 0;
    const sent: number[] = [];
    const outputs: string[] = [];
    const sender = new ChunkSender(
      {
        isLive: () => true,
        send: (b) => {
          sent.push(b[0]!);
          return ++n === 1 ? Promise.reject(new Error("ipc down")) : Promise.resolve("ok");
        },
        onOutput: (o) => outputs.push(o),
      },
      8,
      30,
    );
    sender.push(block(1));
    sender.push(block(2));
    await sleep(20);

    expect(sent).toEqual([1, 2]); // failure didn't stall the drain
    expect(sender.droppedBlocks).toBe(1); // the failed call's block is counted lost
    expect(outputs).toEqual(["ok"]);
  });
});
