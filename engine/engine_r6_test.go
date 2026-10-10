package engine

import (
	"math"
	"testing"
	"time"
)

// R6 — player-level underrun: model the actual playback side
// (public/worklet.js) instead of judging engine emit alone. The
// reviewer's point: "the ~85 ms prebuffer absorbs most of the hole" is
// not a worst-case guarantee, because the ring's water level drifts
// with consumption. These tests feed the engine block by block, push
// each Process output into a faithful model of the worklet's sample
// ring, and drain it at the same rate the AudioWorklet consumes —
// then count real starvation.
//
// Worklet semantics mirrored from public/worklet.js:
//   - fixed RING_CAP ring; pushes drop oldest-first past CAP_SAMPLES
//   - output is silence until rCount >= PREBUFFER (prebuffer gate)
//   - one 128-sample read per process() call (one output quantum)
//   - a quantum that finds the ring empty counts one underrun and
//     emits silence for its missing tail; STARVE_REARM consecutive
//     starved quanta re-arm the prebuffer
//   - producer/consumer run at the same wall-clock rate: while the mic
//     captures one 2048 block the player drains 2048 samples. The
//     block's own emit can only land near the START of the NEXT
//     capture period (capture 42.7 ms + processing ~ms), so within a
//     period the consumer drains first and the previous block's emit
//     arrives one quantum in — that ordering is modeled literally.
const (
	wkBlock       = 2048                // capture block (worklet BLOCK)
	wkQuantum     = 128                 // one process() output quantum
	wkRingCap     = SampleRate          // RING_CAP = ~1 s
	wkCapSamples  = SampleRate * 3 / 10 // CAP_SAMPLES = ~300 ms latency ceiling
	wkPrebuffer   = 2 * wkBlock         // PREBUFFER = ~85 ms
	wkStarveRearm = 128                 // STARVE_REARM starved quanta → re-arm
)

// playRing models the worklet's playback ring. Only bookkeeping is
// simulated — underruns depend on sample counts, not content — but
// every rule above is reproduced.
type playRing struct {
	rCount int  // buffered samples (rCount in worklet)
	prebuf bool // prebuffering gate
	strvQ  int  // consecutive starved quanta (starve in worklet)

	q           int   // quanta elapsed
	starvedPerQ []int // starved samples per quantum (0..wkQuantum)
	prebufPerQ  []int // quanta spent gated (logged as wkQuantum)
	levelAtQ    []int // rCount snapshot per quantum (for the report)
	underrunQ   int   // worklet "underruns" counter
	overruns    int   // worklet "overruns" counter (dropped samples)
}

func newPlayRing(fill int) *playRing {
	return &playRing{rCount: fill, prebuf: true}
}

// push appends n produced samples, mirroring port.onmessage: overflow
// past the ring cap and the 300 ms latency ceiling drops oldest-first.
// The dropped count must be captured BEFORE clamping rCount — the
// earlier order computed the drop from an already-clamped rCount and
// silently recorded overruns=0 (R7 review finding).
func (r *playRing) push(n int) {
	r.rCount += n
	if r.rCount > wkRingCap {
		r.overruns += r.rCount - wkRingCap
		r.rCount = wkRingCap
	}
	if r.rCount > wkCapSamples {
		drop := r.rCount - wkCapSamples
		r.rCount = wkCapSamples
		r.overruns += drop
	}
}

// quantum is one worklet process() call: 128 output samples.
func (r *playRing) quantum() {
	if r.prebuf && r.rCount >= wkPrebuffer {
		r.prebuf = false
		r.strvQ = 0
	}
	r.q++
	r.levelAtQ = append(r.levelAtQ, r.rCount)
	if r.prebuf {
		r.starvedPerQ = append(r.starvedPerQ, 0)
		r.prebufPerQ = append(r.prebufPerQ, wkQuantum)
		return
	}
	starved := 0
	for i := 0; i < wkQuantum; i++ {
		if r.rCount > 0 {
			r.rCount--
		} else {
			starved++
		}
	}
	if starved > 0 {
		r.underrunQ++
		r.strvQ++
		if r.strvQ >= wkStarveRearm {
			r.prebuf = true // sustained starve re-arms the prebuffer
		}
	} else {
		r.strvQ = 0
	}
	r.starvedPerQ = append(r.starvedPerQ, starved)
	r.prebufPerQ = append(r.prebufPerQ, 0)
}

// playbackSim streams `in` through the engine in 2048 blocks with a
// pitch switch at switchAt, driving a playRing at the duplex rate.
// Returns the ring plus the per-block emit lengths and the quantum
// index / ring level at the switch instant.
type playbackSim struct {
	ring        *playRing
	emitLen     []int
	inLen       []int
	swBlock     int // block index where SetParams ran
	swQ         int // quantum index where SetParams ran
	levelAtSw   int // rCount when the switch happened
	pushedTotal int
}

func simulatePlayback(from, to float64, fill int) *playbackSim {
	const chunk = wkBlock
	in := vowel(200, 900, 3.0)
	e := New()
	p := DefaultParams
	p.Pitch = from
	e.SetParams(p)
	s := &playbackSim{ring: newPlayRing(fill), swBlock: -1, swQ: -1}
	switchAt := SampleRate
	pending := 0 // previous block's emit, lands ~1 quantum into this period
	for i := 0; i < len(in); i += chunk {
		if i >= switchAt && s.swQ < 0 {
			p.Pitch = to
			e.SetParams(p)
			s.swQ = s.ring.q
			s.swBlock = i / chunk
			s.levelAtSw = s.ring.rCount
		}
		end := min(i+chunk, len(in))
		out := e.Process(in[i:end])
		for qi := 0; qi*wkQuantum < end-i; qi++ {
			if qi == 1 {
				s.ring.push(pending)
				s.pushedTotal += pending
				pending = 0
			}
			s.ring.quantum()
		}
		pending = len(out)
		s.emitLen = append(s.emitLen, len(out))
		s.inLen = append(s.inLen, end-i)
	}
	s.ring.push(pending)
	s.pushedTotal += pending
	return s
}

func sumRange(v []int, a, b int) int {
	if b < 0 || b > len(v) {
		b = len(v)
	}
	s := 0
	for _, x := range v[a:b] {
		s += x
	}
	return s
}

// worstSilence is the longest run of consecutive starved output
// samples. Within a quantum reads always precede starvation (the ring
// only shrinks inside process()), so a partial-quantum starve ends the
// run; only a fully-starved quantum carries it into the next one.
func worstSilence(v []int) int {
	cur, mx := 0, 0
	for _, s := range v {
		cur += s
		if cur > mx {
			mx = cur
		}
		if s < wkQuantum {
			cur = 0
		}
	}
	return mx
}

// TestPlayRingModelOverflow is the R7 regression on the model itself:
// a push past CAP_SAMPLES must record the dropped samples before
// clamping rCount — the earlier order measured the drop from the
// already-clamped rCount and always recorded overruns=0, which would
// also have falsified the conservation check had overflow triggered.
func TestPlayRingModelOverflow(t *testing.T) {
	// reviewer's case: 14000 buffered + 2048 pushed → drop 1648.
	r := newPlayRing(14000)
	r.push(2048)
	if r.rCount != wkCapSamples {
		t.Fatalf("rCount = %d, want %d", r.rCount, wkCapSamples)
	}
	if want := 14000 + 2048 - wkCapSamples; r.overruns != want {
		t.Fatalf("overruns = %d, want %d", r.overruns, want)
	}
	// conservation: init+pushed = pulled+rCount+overruns (nothing pulled)
	if rem := 14000 + 2048 - r.rCount - r.overruns; rem != 0 {
		t.Fatalf("conservation: unpulled remainder %d", rem)
	}
	// wrap path: past RING_CAP the wrap loss and the cap drop both count.
	r2 := newPlayRing(14000)
	r2.push(36000) // 50000: wrap drops 2000, cap drops 33600
	if r2.rCount != wkCapSamples || r2.overruns != 35600 {
		t.Fatalf("wrap+cap: rCount=%d overruns=%d, want %d/%d",
			r2.rCount, r2.overruns, wkCapSamples, 35600)
	}
	// push under the cap drops nothing.
	r3 := newPlayRing(2048)
	r3.push(2048)
	if r3.rCount != 4096 || r3.overruns != 0 {
		t.Fatalf("clean push: rCount=%d overruns=%d", r3.rCount, r3.overruns)
	}
}

// TestSwitchPlaybackUnderrun is the R6 acceptance: the switch hole is
// judged at the player, parameterized by the ring's water level when
// the stream starts — 0 and 2048 sit under the prebuffer gate, 4096 is
// exactly PREBUFFER, 8192 is a healthy buffer, CAP_SAMPLES is full.
// The worklet drops pushes past CAP_SAMPLES, so "full" is 14400.
func TestSwitchPlaybackUnderrun(t *testing.T) {
	const chunk = wkBlock
	for _, dir := range []struct {
		name     string
		from, to float64
		// R7 experience-regression bounds per initial fill:
		// {max post-switch starved samples, max continuous silence}.
		// Baselines measured at this head (dry→wet 2272/2160,
		// wet→wet 608/528) plus margin; 0 where a healthy ring
		// fully absorbed the hole. Improvements stay green — the
		// bound only fails on a real regression vs today.
		exp map[int][2]int
	}{
		{"dry→wet 0→+0.5", 0, 0.5, map[int][2]int{
			0: {2432, 2432}, 2048: {2432, 2432}, wkPrebuffer: {2432, 2432},
			8192: {0, 0}, wkCapSamples: {0, 0},
		}},
		{"wet→wet +0.5→+1", 0.5, 1, map[int][2]int{
			0: {736, 704}, 2048: {736, 704}, wkPrebuffer: {736, 704},
			8192: {0, 0}, wkCapSamples: {0, 0},
		}},
	} {
		t.Logf("=== %s ===", dir.name)
		t.Logf("%8s %8s %8s %8s %8s %10s %10s %9s %9s", "init", "lvl@sw", "postDef", "preStv", "postStv", "underQ", "worstSil", "prebuf", "ovr")
		for _, fill := range []int{0, 2048, wkPrebuffer, 8192, wkCapSamples} {
			s := simulatePlayback(dir.from, dir.to, fill)
			r := s.ring
			preStarved := sumRange(r.starvedPerQ, 0, s.swQ)
			postStarved := sumRange(r.starvedPerQ, s.swQ, -1)
			prebufSil := sumRange(r.prebufPerQ, 0, -1)
			worst := worstSilence(r.starvedPerQ[s.swQ:])
			// post-switch emit deficit: input fed vs samples emitted
			postDef := 0
			for i := s.swBlock; i < len(s.emitLen); i++ {
				postDef += s.inLen[i] - min(s.emitLen[i], s.inLen[i])
			}
			t.Logf("%8d %8d %8d %8d %8d %10d %8.0fms %7.0fms %9d",
				fill, s.levelAtSw, postDef, preStarved, postStarved,
				r.underrunQ, float64(worst)/48, float64(prebufSil)/48, r.overruns)
			// conservation check on the model itself: every consumed
			// sample either came out of the ring or starved.
			pulled := fill + s.pushedTotal - r.rCount - r.overruns
			attempted := 0
			for _, pb := range r.prebufPerQ {
				attempted += wkQuantum - pb
			}
			if got := attempted - pulled; got != preStarved+postStarved {
				t.Fatalf("%s fill=%d: model violated conservation: starved=%d pulled=%d attempted=%d",
					dir.name, fill, preStarved+postStarved, pulled, attempted)
			}
			// A full ring must never starve: the whole stream's deficit
			// (~4-6k) fits well under the cap's 14400.
			if fill == wkCapSamples && preStarved+postStarved != 0 {
				t.Errorf("%s fill=%d: full ring underran %d samples",
					dir.name, fill, preStarved+postStarved)
			}
			// dry→wet is one contiguous hole: the underrun must equal
			// hole minus the reserve held at the switch, within two
			// quanta of granularity — this is the causality identity.
			if dir.from == 0 {
				want := max(0, postDef-s.levelAtSw)
				if d := postStarved - want; d < -2*wkQuantum || d > 2*wkQuantum {
					t.Errorf("%s fill=%d: underrun %d, want ~hole(%d)-reserve(%d)=%d",
						dir.name, fill, postStarved, postDef, s.levelAtSw, want)
				}
			} else if postStarved > postDef+2*chunk {
				// wet→wet emit jitters at hop granularity, so a low
				// reserve starves more than the hole alone; bound it by
				// the deficit plus the in-flight slack.
				t.Errorf("%s fill=%d: underrun %d exceeds deficit(%d)+slack",
					dir.name, fill, postStarved, postDef)
			}
			// Correctness bound: the continuous silence must stay
			// inside the causal bound (new-chain lookahead ~4608 +
			// emit cap + grain) — proves the sim is not stuck.
			if bound := 4608 + stallBlocks*chunk + wkQuantum; worst > bound {
				t.Errorf("%s fill=%d: continuous silence %.0f ms exceeds bound %.0f ms",
					dir.name, fill, float64(worst)/48, float64(bound)/48)
			}
			// R7 experience-regression bound: a much tighter
			// per-fill limit pinned near today's measured values,
			// so a change that worsens the audible underrun fails
			// loudly even while staying under the causal bound.
			if b, ok := dir.exp[fill]; ok {
				if postStarved > b[0] {
					t.Errorf("%s fill=%d: post-switch starved %d exceeds experience bound %d",
						dir.name, fill, postStarved, b[0])
				}
				if worst > b[1] {
					t.Errorf("%s fill=%d: continuous silence %d exceeds experience bound %d",
						dir.name, fill, worst, b[1])
				}
			}
		}
	}
}

// TestPrewarmTransient measures what the R5 prewarm buys in output
// quality: the fade region right after the emit gap is where the new
// chain's cold-start transient would land without prewarm. We compare
// the segment's energy profile prewarm-on vs prewarm-off (prewarmLen
// set to 0 reproduces the R4 cold switch) against a steady reference.
func TestPrewarmTransient(t *testing.T) {
	const chunk = wkBlock
	old := prewarmLen
	defer func() { prewarmLen = old }()
	var warmRatio, coldRatio float64
	for _, warm := range []int{old, 0} {
		prewarmLen = warm
		in := vowel(200, 900, 3.0)
		e := New()
		p := DefaultParams
		e.SetParams(p)
		switchAt := SampleRate
		var out []float32
		var emitLen []int
		swBlock := -1
		for i := 0; i < len(in); i += chunk {
			if i >= switchAt && swBlock < 0 {
				p.Pitch = 0.5
				e.SetParams(p)
				swBlock = i / chunk
			}
			end := min(i+chunk, len(in))
			o := e.Process(in[i:end])
			emitLen = append(emitLen, len(o))
			out = append(out, o...)
		}
		// resume = first post-switch block emitting a full block after
		// the gap; the fade region starts where emit resumes
		resumeB, pos := -1, 0
		acc := 0
		for i := 0; i < len(emitLen); i++ {
			if resumeB < 0 && i > swBlock && emitLen[i] >= chunk*3/4 && emitLen[i-1] < chunk*3/4 {
				resumeB, pos = i, acc
			}
			acc += emitLen[i]
		}
		if resumeB < 0 {
			t.Fatalf("prewarm=%d: emit never resumed after switch", warm)
		}
		seg := out[pos:min(pos+morphFadeLen, len(out))]
		ref := out[len(out)-morphFadeLen:]
		rmsRef := rms32(ref)
		// slide a 128 window over the fade region: the cold transient
		// shows as a sag (min window RMS << steady) or near-zero head
		minRatio, nearZero := math.MaxFloat64, 0
		for i := 0; i+wkQuantum <= len(seg); i += wkQuantum {
			if r := rms32(seg[i:i+wkQuantum]) / rmsRef; r < minRatio {
				minRatio = r
			}
		}
		for _, v := range seg[:min(512, len(seg))] {
			if math.Abs(float64(v)) < 0.02 {
				nearZero++
			}
		}
		gap := 0
		for i := swBlock; i < resumeB; i++ {
			gap += chunk - emitLen[i]
		}
		t.Logf("prewarm=%5d: emit gap %d samp (~%.0f ms), resume@%d; fade-region min window RMS %.2f of steady, near-zero head %d/512, seg RMS %.2f of steady",
			warm, gap, float64(gap)/48, pos, minRatio, nearZero, rms32(seg)/rmsRef)
		if warm == 0 {
			coldRatio = minRatio
		} else {
			warmRatio = minRatio
		}
	}
	// prewarm should never deepen the fade-region sag (loose bound).
	if warmRatio+0.1 < coldRatio {
		t.Errorf("prewarmed fade sags deeper: %.2f < cold %.2f", warmRatio, coldRatio)
	}
}

// TestSetParamsCost times the SetParams lock hold — the same mutex
// Process needs — at pitch extremes, plus a 10-switch burst every two
// blocks. Numbers inform the report; assertions stay loose.
func TestSetParamsCost(t *testing.T) {
	const chunk = wkBlock
	in := vowel(200, 900, 2.0)
	for _, pitch := range []float64{-1, -0.5, 0.5, 1} {
		e := New()
		p := DefaultParams
		e.SetParams(p)
		var ds []time.Duration
		for rep := 0; rep < 5; rep++ {
			// stream ~0.4 s so dryQ holds a full prewarm window
			for i := 0; i < 8*chunk; i += chunk {
				e.Process(in[i%len(in) : min(i%len(in)+chunk, len(in))])
			}
			p.Pitch = 0
			e.SetParams(p) // settle back to dry untimed
			for i := 0; i < 2*chunk; i += chunk {
				e.Process(in[i%len(in) : min(i%len(in)+chunk, len(in))])
			}
			t0 := time.Now()
			p.Pitch = pitch
			e.SetParams(p)
			ds = append(ds, time.Since(t0))
		}
		var tot, mx time.Duration
		for _, d := range ds {
			tot += d
			if d > mx {
				mx = d
			}
		}
		t.Logf("pitch %+.1f: SetParams lock hold mean %.2f ms, max %.2f ms (n=%d)",
			pitch, float64(tot)/float64(len(ds))/1e6, float64(mx)/1e6, len(ds))
	}
	// burst: 10 switches, one every two blocks, alternating between
	// two wet pitches so every SetParams pays a full rebuild+prewarm
	// — the UI-slider storm's worst case. A second pass with prewarm
	// off isolates its share of the lock hold.
	old := prewarmLen
	defer func() { prewarmLen = old }()
	for _, warm := range []int{old, 0} {
		prewarmLen = warm
		e := New()
		p := DefaultParams
		e.SetParams(p)
		var tot, mx time.Duration
		sw := 0
		for i := 0; i+chunk <= len(in) && sw < 10; i += chunk {
			if i > 0 && i/chunk%2 == 0 && i/chunk/2 <= 10 {
				if p.Pitch == 0.25 {
					p.Pitch = 0.75
				} else {
					p.Pitch = 0.25
				}
				t0 := time.Now()
				e.SetParams(p)
				d := time.Since(t0)
				tot += d
				if d > mx {
					mx = d
				}
				sw++
			}
			e.Process(in[i : i+chunk])
		}
		t.Logf("burst(prewarm=%d): %d wet↔wet switches one per 2 blocks: total %.2f ms, mean %.2f ms, peak %.2f ms",
			warm, sw, float64(tot)/1e6, float64(tot)/float64(sw)/1e6, float64(mx)/1e6)
	}
}

func rms32(x []float32) float64 {
	if len(x) == 0 {
		return 0
	}
	var acc float64
	for _, v := range x {
		acc += float64(v) * float64(v)
	}
	return math.Sqrt(acc / float64(len(x)))
}
