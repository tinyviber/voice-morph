import { describe, expect, test } from "bun:test";
import { MonitorSupervisor } from "../src/monitor-queue";
import type { MonitorStartDeps, MonitorResources } from "../src/monitor-queue";

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** a promise the test resolves/rejects by hand — the delayed getUserMedia */
function gate<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e?: unknown) => void;
  const p = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { p, resolve, reject };
}

// ── device fakes (ctx / mic stream / worklet node) ──────────────
function fakeCtx() {
  return {
    closed: false,
    close() {
      this.closed = true;
      return Promise.resolve();
    },
  };
}
function fakeStream() {
  const tracks = [
    { stopped: false, stop() { this.stopped = true; } },
    { stopped: false, stop() { this.stopped = true; } },
  ];
  return { tracks, getTracks: () => tracks };
}
function fakeNode() {
  return { disconnected: false, disconnect() { this.disconnected = true; } };
}

/** the fakes one setup() call acquired — released fields get flagged */
interface Attempt {
  ctx: ReturnType<typeof fakeCtx>;
  stream?: ReturnType<typeof fakeStream>;
  node?: ReturnType<typeof fakeNode>;
}

/**
 * A setup wired like main.ts: ctx first, then await the (gated) mic —
 * so the test controls exactly how far each attempt gets. Fills `res`
 * incrementally, mirroring "assign each field the moment it exists".
 */
function setupWithMic(mic: Promise<Attempt["stream"]>, attempts: Attempt[]) {
  return async (res: MonitorResources) => {
    const a: Attempt = { ctx: fakeCtx() };
    attempts.push(a);
    res.ctx = a.ctx;
    a.stream = await mic; // getUserMedia gate
    res.stream = a.stream;
    a.node = fakeNode();
    res.node = a.node;
  };
}

describe("MonitorSupervisor", () => {
  test("连点两次：第二个 start 直接 busy，只有一次初始化走完并提交", async () => {
    const monitor = new MonitorSupervisor();
    const mic = gate<Attempt["stream"]>();
    const attempts: Attempt[] = [];
    let resets = 0;
    const deps: MonitorStartDeps = {
      resetStream: () => {
        resets++;
        return sleep(5).then(() => 4000);
      },
      setup: setupWithMic(mic.p, attempts),
    };

    const first = monitor.start(deps);
    const second = monitor.start(deps); // 连点

    expect(await second).toBe("busy"); // 初始化期间再次点击 = no-op
    expect(resets).toBe(1); // 第二次连 ResetStream 都没发

    mic.resolve(fakeStream());
    expect(await first).toBe("started");
    expect(attempts).toHaveLength(1); // 只有一次真的走过创建流程
    expect(monitor.session?.ctx).toBe(attempts[0]!.ctx);
    expect(monitor.running).toBe(true);
  });

  test("第一次启动中途失败后重试：失败清理只动自己的资源", async () => {
    const monitor = new MonitorSupervisor();
    const attempts: Attempt[] = [];
    let calls = 0;
    const deps: MonitorStartDeps = {
      resetStream: () => Promise.resolve(4000),
      setup: async (res) => {
        calls++;
        const a: Attempt = { ctx: fakeCtx() };
        attempts.push(a);
        res.ctx = a.ctx;
        if (calls === 1) {
          // getUserMedia 被拒：ctx 已建、mic 未开
          throw new Error("mic denied");
        }
        a.stream = fakeStream();
        res.stream = a.stream;
        a.node = fakeNode();
        res.node = a.node;
      },
    };

    await expect(monitor.start(deps)).rejects.toThrow("mic denied");
    // 失败路径只释放了它自己拿到的 ctx —— 不会动任何别人的设备
    expect(attempts[0]!.ctx.closed).toBe(true);
    expect(monitor.session).toBeUndefined();
    expect(monitor.running).toBe(false);

    expect(await monitor.start(deps)).toBe("started");
    // 第二次成功 session 的设备没有被第一次的失败清理波及
    expect(attempts[1]!.ctx.closed).toBe(false);
    expect(attempts[1]!.stream!.tracks.every((t) => !t.stopped)).toBe(true);
    expect(attempts[1]!.node!.disconnected).toBe(false);
    expect(monitor.session?.ctx).toBe(attempts[1]!.ctx);
    expect(monitor.session?.stream).toBe(attempts[1]!.stream);
  });

  test("start 进行中 stop()：过期 attempt 提交前自检失效，只释放自己的资源", async () => {
    const monitor = new MonitorSupervisor();
    const mic = gate<Attempt["stream"]>();
    const attempts: Attempt[] = [];
    let isLive: (() => boolean) | undefined;
    const deps: MonitorStartDeps = {
      resetStream: () => Promise.resolve(4000),
      setup: async (res, _base, live) => {
        isLive = live;
        const a: Attempt = { ctx: fakeCtx() };
        attempts.push(a);
        res.ctx = a.ctx;
        a.stream = await mic.p;
        res.stream = a.stream;
        a.node = fakeNode();
        res.node = a.node;
      },
    };

    const starting = monitor.start(deps);
    await sleep(1); // setup 已跑到 mic gate
    expect(isLive!()).toBe(true);

    monitor.stop(); // session 尚未提交，但 generation 立即作废
    expect(isLive!()).toBe(false);

    mic.resolve(fakeStream()); // 晚到的 mic 授权
    expect(await starting).toBe("stale");

    // 没有提交 session；过期 attempt 拿到的设备全部自己释放
    expect(monitor.session).toBeUndefined();
    expect(attempts[0]!.ctx.closed).toBe(true);
    expect(attempts[0]!.stream!.tracks.every((t) => t.stopped)).toBe(true);
    expect(attempts[0]!.node!.disconnected).toBe(true);
  });

  test("stop 释放已提交 session 的全部设备并失效其 isLive", async () => {
    const monitor = new MonitorSupervisor();
    const attempts: Attempt[] = [];
    let isLive: (() => boolean) | undefined;
    const deps: MonitorStartDeps = {
      resetStream: () => Promise.resolve(4000),
      setup: async (res, _base, live) => {
        isLive = live;
        const a: Attempt = { ctx: fakeCtx() };
        attempts.push(a);
        res.ctx = a.ctx;
        a.stream = fakeStream();
        res.stream = a.stream;
        a.node = fakeNode();
        res.node = a.node;
      },
    };

    expect(await monitor.start(deps)).toBe("started");
    expect(isLive!()).toBe(true);

    monitor.stop();
    expect(isLive!()).toBe(false);
    expect(monitor.running).toBe(false);
    expect(monitor.session).toBeUndefined();
    expect(attempts[0]!.ctx.closed).toBe(true);
    expect(attempts[0]!.stream!.tracks.every((t) => t.stopped)).toBe(true);
    expect(attempts[0]!.node!.disconnected).toBe(true);
  });
});
