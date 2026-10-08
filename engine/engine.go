// Package engine implements the VoiceMorph DSP chain in pure Go:
//
//	pre-EQ → pitch shift → timbre (formant) shift → post-EQ → gain/limiter
//
// Pitch shift is resample + WSOLA time-stretch: resampling by r scales
// pitch and formants together while shrinking the duration, and WSOLA
// stretches the result back to the original length. Timbre shift then
// warps the spectral envelope independently of pitch, the MorphVOX split
// of "pitch" vs "harmonic quality".
package engine

import (
	"math"
	"sync"
)

// SampleRate is the fixed engine rate; the frontend resamples mic input.
const SampleRate = 48000

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

// Engine holds one streaming instance of the chain. Process consumes
// float32 mono 48 kHz blocks and returns the same-rate morphed block;
// internal latency (~150 ms) shows up as delayed first output.
type Engine struct {
	mu     sync.Mutex
	params Params

	pre, post *EQ
	res       *Resampler
	wso       *WSOLA
	form      *Formant

	pitchR, timbreW, strength float64
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
// streaming state (no click), changed morph stages are rebuilt.
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
	if r != e.pitchR {
		e.res = NewResampler(1 / r)                      // shrink duration by r
		e.wso = NewWSOLA(1/r, 2048, 512, 256, int(e.sr)) // stretch back by r
		e.pitchR = r
	}
	if w != e.timbreW || p.Strength != e.strength {
		e.form = NewFormant(w, p.Strength, int(e.sr))
		e.timbreW, e.strength = w, p.Strength
	}
	e.params = p
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
	e.res = NewResampler(1 / r)
	e.wso = NewWSOLA(1/r, 2048, 512, 256, int(e.sr))
	e.pitchR = r
	e.form = NewFormant(math.Pow(2, e.params.Timbre), e.params.Strength, int(e.sr))
	e.timbreW, e.strength = math.Pow(2, e.params.Timbre), e.params.Strength
}

// morphActive reports whether the pitch/timbre section does any work;
// at neutral params it is skipped entirely (lower latency, truly
// transparent "原声").
func (e *Engine) morphActive() bool {
	return e.pitchR != 1 || e.timbreW != 1
}

// Process runs one block through the chain.
func (e *Engine) Process(in []float32) []float32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.params.Bypass {
		return append([]float32(nil), in...)
	}
	x := make([]float64, len(in))
	for i, v := range in {
		x[i] = float64(v)
	}
	x = e.pre.Process(x)
	if e.morphActive() {
		x = e.res.Process(x)
		x = e.wso.Process(x)
		x = e.form.Process(x)
	}
	x = e.post.Process(x)
	g := e.params.Gain
	out := make([]float32, len(x))
	for i, v := range x {
		out[i] = float32(softClip(v * g))
	}
	return out
}

// Flush drains the streaming stages' tails and resets the engine, so a
// live session can end cleanly. Terminal for the current stream.
func (e *Engine) Flush() []float32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []float32
	if !e.params.Bypass && e.morphActive() {
		x := e.res.Flush()
		x = e.wso.Process(x)
		x = append(x, e.wso.Flush()...)
		x = e.form.Process(x)
		x = append(x, e.form.Flush()...)
		x = e.post.Process(x)
		g := e.params.Gain
		out = make([]float32, len(x))
		for i, v := range x {
			out[i] = float32(softClip(v * g))
		}
	}
	e.rebuildLocked()
	return out
}

// Render processes a whole clip offline, flushing every stage's tail.
func (e *Engine) Render(in []float32) []float32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.params.Bypass {
		return append([]float32(nil), in...)
	}
	x := make([]float64, len(in))
	for i, v := range in {
		x[i] = float64(v)
	}
	x = e.pre.Process(x)
	if e.morphActive() {
		x = append(e.res.Process(x), e.res.Flush()...)
		x = append(e.wso.Process(x), e.wso.Flush()...)
		x = append(e.form.Process(x), e.form.Flush()...)
	}
	x = e.post.Process(x)
	g := e.params.Gain
	out := make([]float32, len(x))
	for i, v := range x {
		out[i] = float32(softClip(v * g))
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
