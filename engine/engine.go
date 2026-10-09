// Package engine implements the VoiceMorph DSP chain in pure Go:
//
//	pre-EQ → pitch shift → timbre (formant) shift → post-EQ → gain/limiter
//
// Pitch shift is resample + WSOLA time-stretch: resampling by r scales
// pitch and formants together while shrinking the duration, and WSOLA
// stretches the result back to the original length. The formant stage
// then warps the spectral envelope by w/r, so the net formant shift is
// w alone — pitch moves f0 only, timbre moves formants only: the two
// knobs are orthogonal, the MorphVOX split of "pitch" vs "harmonic
// quality".
package engine

import (
	"math"
	"sync"
)

// SampleRate is the fixed engine rate; the frontend resamples mic input.
const SampleRate = 48000

// morphFadeLen is the crossfade length in samples (~30 ms) used when the
// morph chain is rebuilt or the bypass is toggled. For the morph-side
// fade it is measured in output positions the new side covers: the old
// side keeps covering the stream during the new chain's lookahead
// (~60-130 ms) and blending only starts where both sides cover the
// same input position.
const morphFadeLen = 1440

// Params mirrors the MorphVOX Tweak Panel: pitch and timbre in ±1 units,
// strength 0..1, plus a 10-band graphic EQ on both sides of the morph.
type Params struct {
	Pitch    float64     `json:"pitch"`    // octaves, -1..1 (ratio = 2^pitch)
	Timbre   float64     `json:"timbre"`   // formant warp, -1..1 (ratio = 2^timbre)
	Strength float64     `json:"strength"` // timbre amount, 0..1
	Gain     float64     `json:"gain"`     // output gain 0..2
	Bypass   bool        `json:"bypass"`   // dry pass-through
	EqPre    [10]float64 `json:"eqPre"`    // dB per band
	EqPost   [10]float64 `json:"eqPost"`   // dB per band
}

// DefaultParams is the neutral voice.
var DefaultParams = Params{Strength: 1, Gain: 1}

// morphChain bundles the stages that are rebuilt together on a morph
// parameter change: resampler + WSOLA (pitch) + formant warp (timbre).
type morphChain struct {
	res  *Resampler
	wso  *WSOLA
	form *Formant
}

func (c *morphChain) process(x []float64) []float64 {
	x = c.res.Process(x)
	x = c.wso.Process(x)
	return c.form.Process(x)
}

func (c *morphChain) flush() []float64 {
	x := c.res.Flush()
	x = c.wso.Process(x)
	x = append(x, c.wso.Flush()...)
	x = c.form.Process(x)
	return append(x, c.form.Flush()...)
}

// side is one branch of the morph-section crossfade: a full chain, or
// the unmodified signal while pitch and timbre are neutral
// (chain == nil, the transparent "原声" path).
type side struct {
	chain *morphChain
	q     []float64 // emitted output not yet consumed by the fader
	pos   int       // absolute input position covered by q[0]; both
	// chains preserve the input timeline 1:1, so pos advances exactly
	// as the fader consumes samples — for a chain created at input
	// index S its first emitted sample covers ≈S
}

func (s *side) push(x []float64) {
	if s.chain != nil {
		x = s.chain.process(x)
	}
	s.q = append(s.q, x...)
}

// Engine holds one streaming instance of the chain. Process consumes
// float32 mono 48 kHz blocks and returns the same-rate morphed block;
// internal latency (~150 ms) shows up as delayed first output.
type Engine struct {
	mu     sync.Mutex
	params Params

	pre, post *EQ

	cur  *side // active morph side
	prev *side // side fading out; nil when no transition is running
	fade int   // cur samples already emitted into the crossfade

	bPrevQ  []float64 // bypass crossfade: outgoing mode's pending samples
	bFade   int       // blended samples so far
	bFading bool

	pitchR, timbreW, strength float64
	inPos, outPos             int // streamed input / emitted output
	sr                        float64
}

func New() *Engine {
	e := &Engine{sr: SampleRate}
	e.pre = NewEQ(DefaultParams.EqPre, e.sr)
	e.post = NewEQ(DefaultParams.EqPost, e.sr)
	e.setParamsLocked(DefaultParams)
	return e
}

// SetParams swaps in a new parameter set; unchanged stages keep their
// streaming state (no click), changed morph stages crossfade over ~30 ms.
func (e *Engine) SetParams(p Params) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setParamsLocked(p)
}

func (e *Engine) setParamsLocked(p Params) {
	p.Pitch = clamp(p.Pitch, -1, 1)
	p.Timbre = clamp(p.Timbre, -1, 1)
	p.Strength = clamp(p.Strength, 0, 1)
	p.Gain = clamp(p.Gain, 0, 2)
	r := math.Pow(2, p.Pitch)
	w := math.Pow(2, p.Timbre)
	e.pre.SetGains(p.EqPre, e.sr)
	e.post.SetGains(p.EqPost, e.sr)
	if r != e.pitchR || w != e.timbreW || p.Strength != e.strength {
		// EQ and gain ride along without a rebuild; a morph change
		// swaps in a fresh chain and fades the previous side out.
		nc := e.newChainLocked(r, w, p.Strength)
		if e.cur != nil {
			if e.prev != nil && e.fade == 0 {
				// A transition was already in flight and cur never
				// produced output: keep the covering side alive,
				// drop the in-between side outright — everything it
				// could still emit is redundant with the side that is
				// actually speaking.
			} else {
				e.prev = e.cur
			}
			e.fade = 0
		}
		e.cur = &side{chain: nc, pos: e.inPos}
		e.pitchR, e.timbreW, e.strength = r, w, p.Strength
	}
	if p.Bypass != e.params.Bypass {
		e.bFading, e.bFade = true, 0
	}
	e.params = p
}

// newChainLocked builds the morph chain for r/w/strength, or nil when
// the morph section is neutral. The user-facing timbre warp is
// w^strength (strength scales the effect, not the correctness): the
// formant stage is fed w^strength/r at full strength — resampling by r
// scales formants by r too, so warping back by exactly 1/r leaves the
// net formant shift at w^strength and pitch never drags timbre with it.
func (e *Engine) newChainLocked(r, w, strength float64) *morphChain {
	we := math.Pow(w, strength) // effective warp = 2^(timbre*strength)
	if r == 1 && we == 1 {
		return nil
	}
	return &morphChain{
		res:  NewResampler(1 / r),                      // shrink duration by r
		wso:  NewWSOLA(1/r, 2048, 512, 256, int(e.sr)), // stretch back by r
		form: NewFormant(we/r, 1, int(e.sr)),           // pitch compensation at full strength
	}
}

// Params returns the current set.
func (e *Engine) Params() Params {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.params
}

// Reset discards streaming state (called when monitoring restarts).
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rebuildLocked()
}

// rebuildLocked re-creates every streaming stage under current params.
func (e *Engine) rebuildLocked() {
	e.pre = NewEQ(e.params.EqPre, e.sr)
	e.post = NewEQ(e.params.EqPost, e.sr)
	r := math.Pow(2, e.params.Pitch)
	w := math.Pow(2, e.params.Timbre)
	e.inPos, e.outPos = 0, 0
	e.cur = &side{chain: e.newChainLocked(r, w, e.params.Strength), pos: 0}
	e.prev = nil
	e.fade = 0
	e.bFading, e.bFade, e.bPrevQ = false, 0, nil
	e.pitchR, e.timbreW, e.strength = r, w, e.params.Strength
}

// morphActive reports whether the pitch/timbre section does any work.
func (e *Engine) morphActive() bool {
	return e.cur != nil && e.cur.chain != nil
}

// drainMorph emits the morph section's blended output for this block.
// While a transition runs, output positions are filled by the side that
// covers them: positions only prev covers (pre-switch content, or the
// new chain's warmup window) take prev raw, positions both sides cover
// blend linearly over morphFadeLen, and positions past the fade take
// cur raw. Coverage is keyed by absolute input position — cur samples
// covering already-emitted positions are stale duplicates and dropped,
// so the blend can never replay content ~latency later.
func (e *Engine) drainMorph() []float64 {
	cur := e.cur.q
	e.cur.q = nil
	if e.prev == nil {
		e.cur.pos += len(cur)
		return cur
	}
	emit := e.outPos // next output position to fill
	var out []float64
	// drop stale coverage: prev kept speaking while cur warmed up, so
	// the head of either queue may cover positions already emitted
	e.prev.pos += dropHead(&e.prev.q, emit-e.prev.pos)
	c0 := 0 // first unconsumed index in cur
	if d := emit - e.cur.pos; d > 0 {
		c0 = min(d, len(cur))
		e.cur.pos += c0
	}
	for {
		if e.fade >= morphFadeLen {
			e.prev = nil // fade done: retire the old side and its tail
			break
		}
		pi, ci := emit-e.prev.pos, emit-e.cur.pos+c0
		hasP := pi >= 0 && pi < len(e.prev.q)
		hasC := ci >= c0 && ci < len(cur)
		if !hasP && !hasC {
			break // neither side covers emit yet — resume next block
		}
		switch {
		case hasP && hasC:
			a := float64(e.fade) / morphFadeLen
			out = append(out, e.prev.q[pi]*(1-a)+cur[ci]*a)
			e.fade++
		case hasP:
			// cur does not cover emit yet: emit prev raw, and do not
			// spend fade window — it is measured in cur-covered
			// positions, not elapsed output
			out = append(out, e.prev.q[pi])
		default:
			out = append(out, cur[ci])
			e.fade++
		}
		emit++
	}
	if e.prev == nil {
		// transition over: everything cur still covers emits raw; its
		// coverage is contiguous from emit once it spoke
		if keep := emit - e.cur.pos + c0; keep < len(cur) {
			if keep > c0 {
				out = append(out, cur[keep:]...)
			}
		}
		e.cur.pos += len(cur) - c0
		e.cur.q = nil
	} else {
		// transition still running: carry un-emitted coverage (with
		// positions) into the next block
		keep := emit - e.cur.pos + c0
		if keep < c0 {
			keep = c0
		}
		if keep > len(cur) {
			keep = len(cur)
		}
		e.cur.q = cur[keep:]
		e.cur.pos += keep - c0
		e.prev.pos += dropHead(&e.prev.q, emit-e.prev.pos)
	}
	return out
}

// dropHead removes up to n samples from the head of a queue and returns
// how many were dropped.
func dropHead(q *[]float64, n int) int {
	if n > len(*q) {
		n = len(*q)
	}
	if n < 0 {
		n = 0
	}
	*q = (*q)[n:]
	return n
}

// mixPair drains pending prev output against this block's cur output.
// Pairs blend linearly (α = done/morphFadeLen, measured in cur's
// emitted samples per the spec); while cur is still warming up prev is
// emitted raw, and if prev underflows cur passes unblended. Returns the
// emitted samples and how much of cur was consumed.
func mixPair(pq *[]float64, cur []float64, done *int) (out []float64, used int) {
	p := *pq
	pi := 0
	for *done < morphFadeLen && (pi < len(p) || used < len(cur)) {
		switch {
		case used >= len(cur):
			out = append(out, p[pi])
			pi++
		case pi >= len(p):
			out = append(out, cur[used])
			used++
			*done++
		default:
			a := float64(*done) / morphFadeLen
			out = append(out, p[pi]*(1-a)+cur[used]*a)
			pi++
			used++
			*done++
		}
	}
	*pq = p[pi:]
	return out, used
}

// Process runs one block through the chain.
func (e *Engine) Process(in []float32) []float32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inPos += len(in)
	x := make([]float64, len(in))
	for i, v := range in {
		x[i] = float64(v)
	}
	dry := x
	x = e.pre.Process(x)
	e.cur.push(x)
	if e.prev != nil {
		e.prev.push(x)
	}
	mix := e.drainMorph()
	if e.params.Bypass && !e.bFading {
		// dry pass-through; the morph sides keep running so queues
		// stay bounded and un-bypassing blends immediately
		e.outPos += len(in)
		return append([]float32(nil), in...)
	}
	mix = e.post.Process(mix)
	g := e.params.Gain
	for i, v := range mix {
		mix[i] = softClip(v * g)
	}
	var out []float64
	if e.bFading {
		cur, prev := mix, dry
		if e.params.Bypass {
			cur, prev = dry, mix
		}
		e.bPrevQ = append(e.bPrevQ, prev...)
		var used int
		out, used = mixPair(&e.bPrevQ, cur, &e.bFade)
		out = append(out, cur[used:]...)
		if e.bFade >= morphFadeLen {
			e.bFading, e.bPrevQ = false, nil
		}
	} else {
		out = mix
	}
	e.outPos += len(out)
	fout := make([]float32, len(out))
	for i, v := range out {
		fout[i] = float32(v)
	}
	return fout
}

// Flush drains the streaming stages' tails and resets the engine, so a
// live session can end cleanly. Terminal for the current stream.
func (e *Engine) Flush() []float32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []float64
	if !e.params.Bypass {
		if e.prev != nil && e.prev.chain != nil {
			e.prev.q = append(e.prev.q, e.prev.chain.flush()...)
		}
		if e.cur.chain != nil {
			e.cur.q = append(e.cur.q, e.cur.chain.flush()...)
		}
		mix := e.drainMorph()
		mix = e.post.Process(mix)
		g := e.params.Gain
		out = make([]float64, len(mix))
		for i, v := range mix {
			out[i] = softClip(v * g)
		}
	}
	// keep cumulative output 1:1 with input: a flush may not add or
	// lose tail content relative to Render
	if rem := e.inPos - e.outPos; rem > 0 {
		if len(out) > rem {
			out = out[:rem]
		}
		for len(out) < rem {
			out = append(out, 0)
		}
	} else {
		out = nil
	}
	e.outPos += len(out)
	fout := make([]float32, len(out))
	for i, v := range out {
		fout[i] = float32(v)
	}
	e.rebuildLocked()
	return fout
}

// morphOffset is the measured constant delay (samples at 48 kHz)
// between an input onset and where it lands in the morph chain's
// output, as a function of the pitch ratio r. Measured on
// silence→burst onsets: WSOLA grain search lags ~delta behind nominal
// while compressing (r < 1, offset ≈ +240), and the stretch-time
// overlap leaks onsets forward linearly in r while stretching (r > 1).
// Timbre's own contribution stays within ±3 ms and is not modeled.
func morphOffset(r float64) int {
	if r <= 1 {
		return 240
	}
	return int(240 - 1584*(r-1))
}

// envSeries returns |x| boxcar-smoothed over win samples and subsampled
// by hop — a ~1 kHz energy envelope for onset alignment.
func envSeries(x []float64, win, hop int) []float64 {
	n := len(x) / hop
	if n == 0 {
		return nil
	}
	env := make([]float64, n)
	var acc float64
	j := 0
	for i, v := range x {
		acc += math.Abs(v)
		if i >= win {
			acc -= math.Abs(x[i-win])
		}
		if i%hop == hop-1 && j < n {
			env[j] = acc
			j++
		}
	}
	return env
}

// alignLag measures the pipeline's onset offset: the lag within
// ±960 samples of prior at which the input's energy envelope best
// matches the pipeline output's. Degenerate inputs (silence, flat
// envelopes) fall back to the fitted constant.
func alignLag(xIn, mix []float64, prior int) int {
	const hop = 24 // 0.5 ms at 48 kHz
	ei, eo := envSeries(xIn, 2*hop, hop), envSeries(mix, 2*hop, hop)
	var ei2, eo2 float64
	for _, v := range ei {
		ei2 += v * v
	}
	for _, v := range eo {
		eo2 += v * v
	}
	if ei2 < 1e-9 || eo2 < 1e-9 {
		return prior // silence: nothing to align
	}
	lo, hi := (prior-960)/hop-1, (prior+960)/hop+1
	bestL, bestC := prior/hop, -1.0
	for l := lo; l <= hi; l++ {
		var c float64
		for i := 0; i < len(ei); i++ {
			if j := i + l; j >= 0 && j < len(eo) {
				c += ei[i] * eo[j]
			}
		}
		if c > bestC {
			bestC, bestL = c, l
		}
	}
	if bestC <= 0 {
		return prior
	}
	// ambiguous scores (flat regions, periodic content): prefer the
	// lag closest to the fitted constant
	pick := bestL
	for l := lo; l <= hi; l++ {
		var c float64
		for i := 0; i < len(ei); i++ {
			if j := i + l; j >= 0 && j < len(eo) {
				c += ei[i] * eo[j]
			}
		}
		if c >= 0.98*bestC && abs(l-prior/hop) < abs(pick-prior/hop) {
			pick = l
		}
	}
	return pick * hop
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// shiftIndex maps an output position to the pipeline index Render reads.
// A negative offset (the pipeline leaks onsets forward) would zero-fill
// the first |off| samples and lose the clip's head — but the stream's
// first emitted output inherently covers input ~0. So the applied shift
// ramps linearly 0→off over ~6|off| positions: a monotone map that
// time-squeezes the head gently (j ≈ 5i/6) instead of replacing it with
// silence or replaying a slice of it (a hard join would echo ~28 ms).
func shiftIndex(i, off int) int {
	if off >= 0 || i >= -6*off {
		return i + off
	}
	return i + off*i/(-6*off)
}

// Render processes a whole clip offline, flushing every stage's tail.
// The result is lip-synced: it is shifted back by the measured pipeline
// onset offset and always returns exactly len(in) samples (the residual
// tail is trimmed, a short tail is zero-padded).
func (e *Engine) Render(in []float32) []float32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]float32, len(in))
	if !e.params.Bypass {
		x := make([]float64, len(in))
		for i, v := range in {
			x[i] = float64(v)
		}
		x = e.pre.Process(x)
		// a render is self-contained: discard any transition in flight
		e.prev = nil
		e.fade = morphFadeLen
		e.cur.push(x)
		if e.cur.chain != nil {
			e.cur.q = append(e.cur.q, e.cur.chain.flush()...)
		}
		mix := e.drainMorph()
		mix = e.post.Process(mix)
		g := e.params.Gain
		for i, v := range mix {
			mix[i] = softClip(v * g)
		}
		off := 0
		if e.morphActive() {
			// the true input→output offset is signal- and
			// position-dependent: measure it by cross-correlating the
			// input's and the pipeline output's energy envelopes,
			// constrained to a window around the fitted constant
			off = alignLag(x, mix, morphOffset(e.pitchR))
		}
		for i := range out {
			j := shiftIndex(i, off)
			if j >= 0 && j < len(mix) {
				out[i] = float32(mix[j])
			}
		}
	} else {
		copy(out, in)
	}
	e.rebuildLocked() // reset streaming state for the next render
	return out
}

// Latency returns the internal algorithmic latency in seconds.
func (e *Engine) Latency() float64 {
	// WSOLA lookahead (frame + delta + hop) + formant frame + resampler taps
	return float64(2048+256+512+2048+64) / e.sr
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}

// softClip keeps output under full scale with a smooth knee at 0.9.
func softClip(x float64) float64 {
	const knee = 0.9
	if x > knee {
		return knee + (1-knee)*math.Tanh((x-knee)/(1-knee))
	}
	if x < -knee {
		return -knee - (1-knee)*math.Tanh((-x-knee)/(1-knee))
	}
	return x
}
