package dubbing

import (
	"fmt"
	"math"

	"voice-morph/engine"
	"voice-morph/evaluation"
)

// LoadWAV48 loads a WAV and resamples to the engine rate (48 kHz mono),
// the rate every dubbing artifact is produced at.
func LoadWAV48(path string) ([]float32, error) {
	x, sr, err := evaluation.LoadWAV(path)
	if err != nil {
		return nil, err
	}
	if sr == engine.SampleRate {
		return x, nil
	}
	r := engine.NewResampler(float64(engine.SampleRate) / float64(sr))
	d := make([]float64, len(x))
	for i, v := range x {
		d[i] = float64(v)
	}
	d = append(r.Process(d), r.Flush()...)
	out := make([]float32, len(d))
	for i, v := range d {
		out[i] = float32(v)
	}
	return out, nil
}

// SaveWAV writes a 48 kHz mono WAV atomically.
func SaveWAV(path string, x []float32) error {
	return WriteFileAtomic(path, engine.EncodeWAV(x, engine.SampleRate), 0o644)
}

// MsToSamples converts milliseconds to 48 kHz sample indices, clamped.
func MsToSamples(ms, total int) int {
	n := int(float64(ms) / 1000 * engine.SampleRate)
	if n < 0 {
		return 0
	}
	if n > total {
		return total
	}
	return n
}

// energyRMS returns a short-time RMS envelope: one value per hop of
// hopSamp samples over a winSamp window.
func energyRMS(x []float32, winSamp, hopSamp int) []float64 {
	if len(x) == 0 {
		return nil
	}
	n := (len(x) + hopSamp - 1) / hopSamp
	env := make([]float64, n)
	for i := 0; i < n; i++ {
		a := i * hopSamp
		b := a + winSamp
		if b > len(x) {
			b = len(x)
		}
		var s float64
		for _, v := range x[a:b] {
			s += float64(v) * float64(v)
		}
		if b > a {
			env[i] = math.Sqrt(s / float64(b-a))
		}
	}
	return env
}

// refineBoundary searches [center-pre, center+post] (sample indices,
// clamped to [lo,hi]) for the lowest-energy point — a cut always lands
// in a quiet spot, never mid-phoneme.
func refineBoundary(env []float64, hopSamp, center, pre, post, lo, hi int) int {
	a := center - pre
	if a < lo {
		a = lo
	}
	b := center + post
	if b > hi {
		b = hi
	}
	if a > b {
		a, b = b, a
	}
	ia, ib := a/hopSamp, b/hopSamp
	if ib >= len(env) {
		ib = len(env) - 1
	}
	if ia > ib {
		ia = ib
	}
	best, bv := center, math.Inf(1)
	for i := ia; i <= ib; i++ {
		if env[i] < bv || (env[i] == bv && absInt(i*hopSamp-center) < absInt(best-center)) {
			bv, best = env[i], i*hopSamp
		}
	}
	return best
}

// applyEdgeFades applies a raised-cosine fade of n samples at both ends.
func applyEdgeFades(x []float32, n int) {
	if n <= 0 || len(x) == 0 {
		return
	}
	if 2*n > len(x) {
		n = len(x) / 2
	}
	for i := 0; i < n; i++ {
		w := 0.5 * (1 - math.Cos(math.Pi*float64(i)/float64(n)))
		x[i] *= float32(w)
		x[len(x)-1-i] *= float32(w)
	}
}

// slice clamps [a,b) into x's bounds and copies it out.
func slice(x []float32, a, b int) []float32 {
	if a < 0 {
		a = 0
	}
	if b > len(x) {
		b = len(x)
	}
	if a >= b {
		return nil
	}
	out := make([]float32, b-a)
	copy(out, x[a:b])
	return out
}

// mix adds src into dst starting at sample `at`, truncating to dst.
func mix(dst, src []float32, at int) {
	if at < 0 {
		src = src[-at:]
		at = 0
	}
	if at >= len(dst) {
		return
	}
	n := len(src)
	if at+n > len(dst) {
		n = len(dst) - at
	}
	for i := 0; i < n; i++ {
		dst[at+i] += src[i]
	}
}

// crossfadeInto writes src into dst at `at`, linearly crossfading over
// `fade` samples where dst already has content (overlap with previous
// writes tracked by `filled`).
func crossfadeInto(dst, src []float32, at, fade int, filled []bool) {
	for i := 0; i < len(src) && at+i < len(dst); i++ {
		d := at + i
		if d < 0 {
			continue
		}
		if filled[d] && i < fade && fade > 0 {
			w := float32(i) / float32(fade)
			dst[d] = dst[d]*(1-w) + src[i]*w
		} else {
			dst[d] = src[i]
		}
		filled[d] = true
	}
}

func rmsOf(x []float32) float64 {
	if len(x) == 0 {
		return 0
	}
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

func peakOf(x []float32) float32 {
	var p float32
	for _, v := range x {
		if a := float32(math.Abs(float64(v))); a > p {
			p = a
		}
	}
	return p
}

var _ = fmt.Sprintf // silence unused-import if helpers evolve
