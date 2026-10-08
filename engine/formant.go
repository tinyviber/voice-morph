package engine

import "math"

// Formant shifts the spectral envelope (timbre) of a stream while keeping
// its pitch: each STFT frame's magnitude is decomposed into a cepstrally
// smoothed envelope E(f) and fine structure; the envelope is warped in
// frequency by `warp` (>1 moves formants up, "child-like"; <1 down,
// "giant-like") and the gain E(warped)/E(original) is applied to the
// frame's complex spectrum, preserving phase and harmonic positions.
//
// strength blends the correction: gain = (Ew/E)^strength.
type Formant struct {
	n        int // FFT size
	hop      int
	lifter   int // cepstral quefrency cutoff, in samples
	warp     float64
	strength float64
	win      []float64
	in       []float64
	base     int // absolute index of in[0]
	pos      int // absolute position of the next frame
	out      []float64
	outBase  int
	spec     []complex128
	env      []float64
	gain     []float64
	cbuf     []complex128
	flushed  bool
}

// NewFormant builds a formant shifter for sample rate sr.
func NewFormant(warp, strength float64, sr int) *Formant {
	n := 2048
	f := &Formant{
		n: n, hop: n / 4, lifter: int(0.002 * float64(sr)), // ~2 ms quefrency
		warp: warp, strength: strength,
		win:  hann(n),
		spec: make([]complex128, n),
		env:  make([]float64, n/2+1),
		gain: make([]float64, n/2+1),
		cbuf: make([]complex128, n),
	}
	return f
}

func hann(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return w
}

// Process appends input and returns available output.
func (f *Formant) Process(in []float64) []float64 {
	f.in = append(f.in, in...)
	var out []float64
	for f.canFrame() {
		f.step()
		out = f.emit(out)
	}
	f.trim()
	return out
}

// emit appends every output position that can no longer be touched by a
// future frame (positions < f.pos, the next frame start).
func (f *Formant) emit(out []float64) []float64 {
	for f.outBase < f.pos && len(f.out) > 0 {
		out = append(out, f.out[0])
		f.out = f.out[1:]
		f.outBase++
	}
	return out
}

// Flush pads the input with silence and emits the rest of the stream.
func (f *Formant) Flush() []float64 {
	f.flushed = true
	var out []float64
	for f.canFrame() {
		f.step()
		out = f.emit(out)
	}
	out = append(out, f.out...)
	return out
}

func (f *Formant) canFrame() bool {
	if f.flushed {
		return f.base+len(f.in) > f.pos
	}
	return f.base+len(f.in)-f.pos >= f.n
}

// at reads input at absolute index i (0 outside the buffer).
func (f *Formant) at(i int) float64 {
	if j := i - f.base; j >= 0 && j < len(f.in) {
		return f.in[j]
	}
	return 0
}

func (f *Formant) step() {
	n, nh := f.n, f.n/2+1
	// windowed frame into spec
	for i := 0; i < n; i++ {
		f.spec[i] = complex(f.at(f.pos+i)*f.win[i], 0)
	}
	fft(f.spec, false)

	// log-magnitude spectrum (one-sided)
	logMag := make([]float64, n)
	for i := 0; i < nh; i++ {
		logMag[i] = math.Log(math.Max(cmplxAbs(f.spec[i]), 1e-8))
	}
	for i := nh; i < n; i++ {
		logMag[i] = logMag[n-i]
	}

	// cepstral smoothing → envelope
	for i := 0; i < n; i++ {
		f.cbuf[i] = complex(logMag[i], 0)
	}
	fft(f.cbuf, true)
	for i := 0; i < n; i++ {
		if i > f.lifter && i < n-f.lifter {
			f.cbuf[i] = 0
		}
	}
	fft(f.cbuf, false)
	for i := 0; i < nh; i++ {
		f.env[i] = real(f.cbuf[i]) // log-envelope
	}

	// envelope warp + strength blend → per-bin gain (log domain)
	for k := 0; k < nh; k++ {
		src := float64(k) / f.warp
		if src > float64(nh-1) {
			src = float64(nh - 1)
		}
		i0 := int(src)
		fr := src - float64(i0)
		i1 := i0 + 1
		if i1 > nh-1 {
			i1 = nh - 1
		}
		warpedEnv := f.env[i0]*(1-fr) + f.env[i1]*fr
		g := (warpedEnv - f.env[k]) * f.strength
		f.gain[k] = math.Exp(g)
	}

	// apply gain, mirror to negative freqs, inverse transform
	for k := 0; k < nh; k++ {
		f.spec[k] *= complex(f.gain[k], 0)
	}
	for k := nh; k < n; k++ {
		f.spec[k] = complex(real(f.spec[n-k]), -imag(f.spec[n-k]))
	}
	fft(f.spec, true)

	// overlap-add into out (positions f.pos .. f.pos+n-1); Hann² sums to
	// 1.5 at hop n/4, so normalize by 2/3
	need := f.pos + n - f.outBase
	for len(f.out) < need {
		f.out = append(f.out, 0)
	}
	for i := 0; i < n; i++ {
		f.out[f.pos-f.outBase+i] += f.win[i] * real(f.spec[i]) * (2.0 / 3.0)
	}
	f.pos += f.hop
}

func (f *Formant) trim() {
	drop := f.pos - f.base - f.n
	if drop <= 0 {
		return
	}
	f.in = append([]float64(nil), f.in[drop:]...)
	f.base += drop
	if dropOut := f.pos - f.outBase - f.n; dropOut > 0 {
		f.out = append([]float64(nil), f.out[dropOut:]...)
		f.outBase += dropOut
	}
}

func cmplxAbs(c complex128) float64 { return math.Hypot(real(c), imag(c)) }
