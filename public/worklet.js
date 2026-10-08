// Duplex AudioWorklet: captures mic into 2048-sample blocks posted to the
// page, and plays back morphed blocks pushed through its port. Output
// underruns emit silence (the ring buffer below absorbs ~4 blocks).
const BLOCK = 2048;

class Duplex extends AudioWorkletProcessor {
  constructor() {
    super();
    this.inq = [];
    this.outq = []; // queue of Float32Array blocks
    this.head = 0;
    this.port.onmessage = (e) => {
      this.outq.push(e.data);
      if (this.outq.length > 16) this.outq.shift(); // bound latency
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
    for (let i = 0; i < n; i++) {
      let s = 0;
      if (this.outq.length) {
        const b = this.outq[0];
        s = b[this.head++];
        if (this.head >= b.length) {
          this.outq.shift();
          this.head = 0;
        }
      }
      for (let ch = 0; ch < out.length; ch++) out[ch][i] = s;
    }
    return true;
  }
}

registerProcessor("duplex", Duplex);
