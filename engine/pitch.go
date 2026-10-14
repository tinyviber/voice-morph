package engine

import "math"

// PitchEstimate describes periodic speech; F0 is zero for unvoiced frames.
// Confidence measures periodicity, not the probability of a speaker category.
type PitchEstimate struct {
	F0         float64 `json:"f0"`
	Confidence float64 `json:"confidence"`
	Voiced     bool    `json:"voiced"`
}

// PitchTracker is an allocation-free YIN-style estimator after its first frame.
// Analyze uses 40–64 ms frames on a fixed hop in streaming callers. Averaging
// before decimation suppresses high-frequency frication; FFT autocorrelation
// avoids a quadratic lag search. Instances belong to one stream.
type PitchTracker struct {
	sr     int
	decim  int
	x      []float64
	energy []float64
	diff   []float64
	cmnd   []float64
	buf    []complex128
}

func NewPitchTracker(sr int) *PitchTracker {
	if sr < 1000 {
		sr = SampleRate
	}
	return &PitchTracker{sr: sr, decim: max(1, sr/12000)}
}

const (
	yinThreshold  = 0.18 // first-dip rule: a clearly periodic frame
	nearThreshold = 0.4  // continuation of a voiced run (hysteresis)
	nearSpan      = 1.26 // ±4 semitones around the hinted period
)

// Analyze estimates 60–600 Hz. Silence, non-finite samples, and frames too
// short to contain two periods at the lower bound return an unvoiced result.
// Frame-local estimates keep the same audio independent of caller chunking;
// trajectory smoothing is the responsibility of the synthesis stage.
func (p *PitchTracker) Analyze(frame []float64) PitchEstimate {
	return p.AnalyzeNear(frame, 0)
}

// AnalyzeNear is Analyze with a continuity prior for streaming callers:
// hint is the previous frame's F0 (0 = none). When the first-dip rule
// finds no clearly periodic lag, the deepest dip within ±4 semitones of
// the hinted period is still accepted up to a relaxed threshold. This is
// hysteresis for voiced runs — breathy or creaky continuation frames stay
// voiced — while clearly periodic frames are decided by the unbiased rule,
// so a hint can never lock a run onto a subharmonic.
func (p *PitchTracker) AnalyzeNear(frame []float64, hint float64) PitchEstimate {
	n := len(frame) / p.decim
	fs := float64(p.sr) / float64(p.decim)
	lo := max(2, int(fs/600))
	hi := min(int(fs/60), n/2-1)
	if hi <= lo {
		return PitchEstimate{}
	}
	p.x = resizeFloat(p.x, n)
	var mean, power float64
	for i := range p.x {
		var v float64
		for j := 0; j < p.decim; j++ {
			s := frame[i*p.decim+j]
			if math.IsNaN(s) || math.IsInf(s, 0) {
				return PitchEstimate{}
			}
			v += s
		}
		v /= float64(p.decim)
		p.x[i] = v
		mean += v
	}
	mean /= float64(n)
	p.energy = resizeFloat(p.energy, n+1)
	p.energy[0] = 0
	nfft := 1
	for nfft < 2*n {
		nfft *= 2
	}
	if cap(p.buf) < nfft {
		p.buf = make([]complex128, nfft)
	} else {
		p.buf = p.buf[:nfft]
		clear(p.buf)
	}
	for i, v := range p.x {
		v -= mean
		power += v * v
		p.energy[i+1] = power
		p.buf[i] = complex(v, 0)
	}
	// -55 dBFS mean-square: a quieter gate let the digital noise floor of a
	// recording (-57 dBFS and below) ring a 60 Hz autocorrelation trough
	// with confidence ~0.9, which the speech stage then voiced and
	// synthesized as a low buzz under silence. Speech with any real
	// periodicity sits far above this floor.
	if power/float64(n) < 3e-6 {
		return PitchEstimate{}
	}
	fft(p.buf, false)
	for i, v := range p.buf {
		p.buf[i] = complex(real(v)*real(v)+imag(v)*imag(v), 0)
	}
	fft(p.buf, true)
	p.diff = resizeFloat(p.diff, hi+2)
	p.cmnd = resizeFloat(p.cmnd, hi+2)
	var cumulative float64
	for lag := 1; lag <= hi+1; lag++ {
		// Both terms cover n-lag samples; normalizing the overlap prevents
		// long lags from winning merely because fewer samples contribute.
		d := (p.energy[n-lag] + power - p.energy[lag] - 2*real(p.buf[lag])) / float64(n-lag)
		d = math.Max(0, d)
		p.diff[lag] = d
		cumulative += d
		p.cmnd[lag] = 1
		if cumulative > 1e-20 {
			p.cmnd[lag] = d * float64(lag) / cumulative
		}
	}
	// The first sufficiently periodic local minimum is the fundamental;
	// choosing the absolute minimum instead systematically prefers octaves.
	p.cmnd[0] = 1
	dip := func(lag int) bool { return p.cmnd[lag] <= p.cmnd[lag-1] && p.cmnd[lag] < p.cmnd[lag+1] }
	pick := 0
	for lag := lo; lag <= hi; lag++ {
		if p.cmnd[lag] < yinThreshold && dip(lag) {
			pick = lag
			break
		}
	}
	if pick == 0 && hint > 0 {
		c := fs / hint
		for lag := max(lo, int(c/nearSpan)); lag <= min(hi, int(c*nearSpan)+1); lag++ {
			if p.cmnd[lag] < nearThreshold && dip(lag) && (pick == 0 || p.cmnd[lag] < p.cmnd[pick]) {
				pick = lag
			}
		}
	}
	if pick == 0 {
		return PitchEstimate{}
	}
	a, b, c := p.cmnd[pick-1], p.cmnd[pick], p.cmnd[pick+1]
	delta := 0.0
	if den := a - 2*b + c; den > 1e-12 {
		delta = clamp(0.5*(a-c)/den, -0.5, 0.5)
	}
	f0 := fs / (float64(pick) + delta)
	confidence := clamp(1-b, 0, 1)
	if f0 < 60 || f0 > 600 {
		return PitchEstimate{}
	}
	return PitchEstimate{F0: f0, Confidence: confidence, Voiced: true}
}

func resizeFloat(x []float64, n int) []float64 {
	if cap(x) < n {
		return make([]float64, n)
	}
	return x[:n]
}
