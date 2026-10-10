// Duplex AudioWorklet: captures mic into 2048-sample blocks posted to the
// page, and plays back morphed audio pushed through its port. Playback
// runs through a fixed-capacity sample ring: silence until a ~85 ms
// prebuffer fills, then one read per output sample. Buffered audio above
// ~300 ms is dropped oldest-first so playback latency stays bounded; a
// sustained underrun re-arms the prebuffer so a starving producer doesn't
// sputter block-by-block.
const BLOCK = 2048;
const SR = typeof sampleRate === "number" ? sampleRate : 48000;
const RING_CAP = Math.round(SR); // ~1 s of audio
const CAP_SAMPLES = Math.round(SR * 0.3); // ~300 ms latency ceiling
const PREBUFFER = 2 * BLOCK; // ~85 ms before first output
const STARVE_REARM = 128; // ~341 ms of starved quanta → re-prebuffer
const STATS_EVERY = Math.round(SR / 128); // ≈ once per second

class Duplex extends AudioWorkletProcessor {
  constructor() {
    super();
    this.inq = [];
    this.ring = new Float32Array(RING_CAP);
    this.rHead = 0; // oldest buffered sample
    this.rTail = 0; // next write slot
    this.rCount = 0; // buffered sample count
    this.prebuffering = true;
    this.underruns = 0; // output quanta that hit an empty buffer
    this.overruns = 0; // samples dropped at the cap
    this.starve = 0; // consecutive starved quanta
    this.statClock = 0;
    this.port.onmessage = (e) => {
      let a = e.data;
      if (!a || !a.length) return;
      if (!(a instanceof Float32Array)) a = Float32Array.from(a);
      if (a.length > RING_CAP) {
        // a burst bigger than the ring: keep only its newest second
        this.overruns += a.length - RING_CAP;
        a = a.subarray(a.length - RING_CAP);
      }
      const first = Math.min(a.length, RING_CAP - this.rTail);
      this.ring.set(a.subarray(0, first), this.rTail);
      if (a.length > first) this.ring.set(a.subarray(first), 0);
      this.rTail = (this.rTail + a.length) % RING_CAP;
      this.rCount += a.length;
      if (this.rCount > RING_CAP) {
        // wrapped past the reader: overwritten samples are lost
        const ov = this.rCount - RING_CAP;
        this.rHead = (this.rHead + ov) % RING_CAP;
        this.rCount = RING_CAP;
        this.overruns += ov;
      }
      if (this.rCount > CAP_SAMPLES) {
        // latency cap: drop the oldest samples
        const drop = this.rCount - CAP_SAMPLES;
        this.rHead = (this.rHead + drop) % RING_CAP;
        this.rCount -= drop;
        this.overruns += drop;
      }
    };
  }

  process(inputs, outputs) {
    const input = inputs[0];
    const out = outputs[0];

    // capture → page
    if (input && input[0]) {
      this.inq.push(...input[0]);
      while (this.inq.length >= BLOCK) {
        const block = Float32Array.from(this.inq.slice(0, BLOCK));
        this.inq = this.inq.slice(BLOCK);
        this.port.postMessage({ in: block }, [block.buffer]);
      }
    }

    // page → playback
    const n = out[0].length;
    if (this.prebuffering && this.rCount >= PREBUFFER) {
      this.prebuffering = false;
      this.starve = 0;
    }
    if (this.prebuffering) {
      for (let ch = 0; ch < out.length; ch++) out[ch].fill(0);
    } else {
      let starved = false;
      for (let i = 0; i < n; i++) {
        let s = 0;
        if (this.rCount > 0) {
          s = this.ring[this.rHead];
          this.rHead++;
          if (this.rHead === RING_CAP) this.rHead = 0;
          this.rCount--;
        } else {
          starved = true;
        }
        for (let ch = 0; ch < out.length; ch++) out[ch][i] = s;
      }
      if (starved) {
        this.underruns++;
        if (++this.starve >= STARVE_REARM) this.prebuffering = true;
      } else {
        this.starve = 0;
      }
    }

    // stats → page
    if (++this.statClock >= STATS_EVERY) {
      this.statClock = 0;
      this.port.postMessage({
        stats: {
          underruns: this.underruns,
          overruns: this.overruns,
          bufferedMs: Math.round((this.rCount / SR) * 1000),
        },
      });
    }
    return true;
  }
}

registerProcessor("duplex", Duplex);
