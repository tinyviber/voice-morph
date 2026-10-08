package engine

import "math"

// EqFreqs are the ISO center frequencies of the 10-band graphic EQ,
// matching MorphVOX's layout.
var EqFreqs = [10]float64{31.25, 62.5, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}

// Biquad is a direct-form-II peaking EQ filter (RBJ cookbook).
type Biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             float64
}

// PeakingEQ returns a peaking biquad: freq Hz, gain dB, Q.
func PeakingEQ(freq, gainDB, q, sr float64) Biquad {
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * freq / sr
	alpha := math.Sin(w0) / (2 * q)
	cosw := math.Cos(w0)
	b0 := 1 + alpha*a
	b1 := -2 * cosw
	b2 := 1 - alpha*a
	a0 := 1 + alpha/a
	a1 := -2 * cosw
	a2 := 1 - alpha/a
	return Biquad{
		b0: b0 / a0, b1: b1 / a0, b2: b2 / a0,
		a1: a1 / a0, a2: a2 / a0,
	}
}

func (b *Biquad) step(x float64) float64 {
	y := b.b0*x + b.z1
	b.z1 = b.b1*x - b.a1*y + b.z2
	b.z2 = b.b2*x - b.a2*y
	return y
}

// EQ is a cascade of the 10 peaking bands.
type EQ struct {
	bands [10]Biquad
}

// NewEQ builds the cascade from per-band gains in dB.
func NewEQ(gains [10]float64, sr float64) *EQ {
	e := &EQ{}
	for i, g := range gains {
		e.bands[i] = PeakingEQ(EqFreqs[i], g, 1.41, sr)
	}
	return e
}

// SetGains updates coefficients in place, keeping filter state.
func (e *EQ) SetGains(gains [10]float64, sr float64) {
	for i, g := range gains {
		b := PeakingEQ(EqFreqs[i], g, 1.41, sr)
		e.bands[i].b0, e.bands[i].b1, e.bands[i].b2 = b.b0, b.b1, b.b2
		e.bands[i].a1, e.bands[i].a2 = b.a1, b.a2
	}
}

func (e *EQ) Process(in []float64) []float64 {
	out := make([]float64, len(in))
	for i, x := range in {
		for j := range e.bands {
			x = e.bands[j].step(x)
		}
		out[i] = x
	}
	return out
}
