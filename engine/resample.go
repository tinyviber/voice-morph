package engine

import "math"

// Resampler is a streaming windowed-sinc resampler. ratio is
// output-samples per input-sample (ratio > 1 upsamples).
type Resampler struct {
	ratio float64
	in    []float64 // pending input, holds absolute positions [base, base+len)
	base  int       // absolute index of in[0]
	pos   float64   // last emitted output position, absolute input coords
	start bool      // pos not yet initialized
}

// NewResampler makes a resampler with a Blackman-sinc kernel, 16 taps per
// side at ratio 1 (widened when decimating, to band-limit).
func NewResampler(ratio float64) *Resampler {
	return &Resampler{ratio: ratio, start: true}
}

func sinc(x float64) float64 {
	if x == 0 {
		return 1
	}
	x *= math.Pi
	return math.Sin(x) / x
}

func blackman(x, w float64) float64 {
	t := (x + w) / (2 * w)
	if t <= 0 || t >= 1 {
		return 0
	}
	return 0.42 - 0.5*math.Cos(2*math.Pi*t) + 0.08*math.Cos(4*math.Pi*t)
}

func (r *Resampler) kernelWidth() float64 {
	w := 16.0
	if r.ratio < 1 {
		w = w / r.ratio
	}
	return w
}

// Process appends input and returns the output samples that became
// available.
func (r *Resampler) Process(in []float64) []float64 {
	r.in = append(r.in, in...)
	if r.start {
		r.pos = -r.kernelWidth()
		r.start = false
	}
	return r.drain(r.base + len(r.in))
}

// Flush drains the tail (zero padding beyond the input end). Terminal:
// create a new Resampler for the next stream.
func (r *Resampler) Flush() []float64 {
	if r.start {
		r.pos = -r.kernelWidth()
		r.start = false
	}
	return r.drain(r.base + len(r.in) + int(r.kernelWidth()) + 2)
}

// drain emits output at pos+step while the whole kernel fits in [base, limit).
func (r *Resampler) drain(limit int) []float64 {
	step := 1 / r.ratio
	w := r.kernelWidth()
	var out []float64
	for {
		p := r.pos + step
		if p+w >= float64(limit) {
			break
		}
		r.pos = p
		out = append(out, r.at(p))
	}
	// pos is absolute; the drop count must be relative to in's window
	keep := int(r.pos-w) - r.base
	if keep > len(r.in) {
		keep = len(r.in)
	}
	if keep > 0 {
		r.in = append([]float64(nil), r.in[keep:]...)
		r.base += keep
	}
	return out
}

// at evaluates the input at absolute position p; out-of-buffer reads are 0.
func (r *Resampler) at(p float64) float64 {
	w := r.kernelWidth()
	scale := math.Min(r.ratio, 1)
	i0 := int(math.Ceil(p - w))
	i1 := int(math.Floor(p + w))
	var sum, wsum float64
	for i := i0; i <= i1; i++ {
		d := p - float64(i)
		k := sinc(d*scale) * blackman(d, w)
		if k == 0 {
			continue
		}
		if j := i - r.base; j >= 0 && j < len(r.in) {
			sum += r.in[j] * k
		}
		wsum += k
	}
	if wsum == 0 {
		return 0
	}
	return sum / wsum
}
