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
// morph chain is rebuilt or the bypass is toggled. It is measured in the
// new side's emitted output: the old side covers the new chain's
// lookahead (~60-130 ms) before the fade starts counting.
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
				// drop the in-between that never spoke (a hole is
				// worse than retiring it early).
			} else {
				e.prev = e.cur
			}
			e.fade = 0
		}
		e.cur = &side{chain: nc}
		e.pitchR, e.timbreW, e.strength = r, w, p.Strength
	}
	if p.Bypass != e.params.Bypass {
		e.bFading, e.bFade = true, 0
	}
	e.params = p
}

// newChainLocked builds the morph chain for r/w/strength, or nil when
// the morph section is neutral. The formant warp is w/r: resampling by
// r scales formants by r too, so warping back by 1/r leaves the net
// formant shift at exactly w — pitch no longer drags timbre with it.
func (e *Engine) newChainLocked(r, w, strength float64) *morphChain {
	if r == 1 && w == 1 {
		return nil
	}
	return &morphChain{
		res:  NewResampler(1 / r),                      // shrink duration by r
		wso:  NewWSOLA(1/r, 2048, 512, 256, int(e.sr)), // stretch back by r
		form: NewFormant(w/r, strength, int(e.sr)),
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
	e.cur = &side{chain: e.newChainLocked(r, w, e.params.Strength)}
	e.prev = nil
	e.fade = 0
	e.bFading, e.bFade, e.bPrevQ = false, 0, nil
	e.pitchR, e.timbreW, e.strength = r, w, e.params.Strength
	e.inPos, e.outPos = 0, 0
}

// morphActive reports whether the pitch/timbre section does any work.
func (e *Engine) morphActive() bool {
	return e.cur != nil && e.cur.chain != nil
}

// drainMorph emits the morph section's blended output for this block.
// While a transition runs, pairs of old/new emissions are linearly
// crossfaded over morphFadeLen of the new side's output; before the new
// chain produces anything the old side alone covers the stream, so a
// parameter change never drops output.
func (e *Engine) drainMorph() []float64 {
	cur := e.cur.q
	e.cur.q = nil
	var out []float64
	if e.prev != nil {
		var used int
		out, used = mixPair(&e.prev.q, cur, &e.fade)
		cur = cur[used:]
		if e.fade >= morphFadeLen {
			e.prev = nil // fade done: retire the old side and its tail
		}
	}
	return append(out, cur...)
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
		if e.prev != nil && len(e.prev.q) > 0 {
			// fade still unfinished at end of stream: the old side's
			// tail covers earlier positions than the new side's, so
			// emit it first
			mix = append(mix, e.prev.q...)
			mix = append(mix, e.cur.q...)
		}
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
			off = morphOffset(e.pitchR)
		}
		for i := range out {
			if j := i + off; j >= 0 && j < len(mix) {
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
