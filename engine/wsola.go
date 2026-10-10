package engine

import "math"

// WSOLA stretches or compresses a stream in time without changing pitch.
// alpha is input-samples per output-sample: output length = input / alpha.
// Grains of frame samples are overlap-added every hs output samples; each
// grain's input position is searched within ±delta of its nominal position
// for maximum similarity with the previous grain's continuation.
type WSOLA struct {
	frame int
	hs    int
	ov    int
	delta int
	alpha float64
	win   []float64
	in    []float64
	base  int     // absolute index of in[0]
	prev  int     // absolute position of the last placed grain (-1: none)
	apos  float64 // accumulating nominal analysis position; independent of
	// prev so that the ±1-period jitter of periodic input does not drift
	out    []float64 // pending output tail (< frame samples before a grain lands)
	energy []float64 // prefix sums of in² for fast NCC denominators
}

// NewWSOLA builds a stretcher. frame should cover ~2 pitch periods of the
// lowest voice (~40 ms at 48 kHz); hs is the synthesis hop.
func NewWSOLA(alpha float64, frame, hs, delta int, sr int) *WSOLA {
	ov := frame - hs
	win := make([]float64, frame)
	// normalize so that overlap-adding grains at every hop reconstructs
	// unit gain: Σ_k w(i-k·hs) = (frame/hs)·mean(w) = 2 for Hann at hs/4
	norm := float64(hs) / float64(frame) / 0.5
	for i := range win {
		win[i] = norm * (0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(frame)))
	}
	return &WSOLA{
		frame: frame, hs: hs, ov: ov, delta: delta,
		alpha: alpha, win: win, prev: -1,
	}
}

func (w *WSOLA) Process(in []float64) []float64 {
	w.in = append(w.in, in...)
	// extend prefix energies
	e := float64(0)
	if len(w.energy) > 0 {
		e = w.energy[len(w.energy)-1]
	}
	for _, v := range in {
		e += v * v
		w.energy = append(w.energy, e)
	}
	return w.drain(false)
}

// Flush drains the stream; positions beyond the input read as zero.
func (w *WSOLA) Flush() []float64 { return w.drain(true) }

func (w *WSOLA) drain(flush bool) []float64 {
	emitted := make([]float64, 0, int(float64(len(w.in))/w.alpha)+2*w.frame)
	avail := w.base + len(w.in) // first unread absolute index
	for {
		if w.prev < 0 {
			if avail-w.base < w.frame && !flush {
				break
			}
			w.place(0)
			emitted = append(emitted, w.out[:w.hs]...)
			w.out = w.out[w.hs:]
			w.prev = 0
			w.apos = float64(w.hs) * w.alpha
			continue
		}
		c := w.apos
		lo := int(math.Ceil(c - float64(w.delta)))
		hi := int(math.Floor(c + float64(w.delta)))
		// grains must strictly advance in the input, or a long overlap
		// region (silence, drones) would loop in place
		if lo <= w.prev {
			lo = w.prev + 1
		}
		if hi < lo {
			hi = lo
		}
		if avail < hi+w.frame && !flush {
			// the furthest candidate's whole frame must be buffered:
			// place() reads [p, p+frame), and a tail read past avail is
			// silently skipped (never re-added later), permanently
			// denting this and the next three hops
			break
		}
		p := w.pick(lo, hi, c)
		w.place(p)
		emitted = append(emitted, w.out[:w.hs]...)
		w.out = w.out[w.hs:]
		w.prev = p
		// advance the nominal position only when a hop was actually
		// emitted — a stalled iteration must not skip c's content
		w.apos = c + float64(w.hs)*w.alpha
		// drop input no grain can reach again; slide the live window
		// down in place instead of re-allocating it every hop
		drop := w.prev - w.base - w.delta - w.frame
		if drop > 0 {
			copy(w.in, w.in[drop:])
			w.in = w.in[:len(w.in)-drop]
			copy(w.energy, w.energy[drop:])
			w.energy = w.energy[:len(w.energy)-drop]
			w.base += drop
			avail = w.base + len(w.in)
		}
		if flush && float64(w.prev)+float64(w.ov) >= float64(avail) {
			break // candidates ran out of input to match
		}
	}
	if flush {
		emitted = append(emitted, w.out...)
		w.out = nil
	}
	return emitted
}

// pick finds the position in [lo, hi] maximizing the normalized
// cross-correlation between the previous grain's continuation and the
// candidate grain's head.
func (w *WSOLA) pick(lo, hi int, c float64) int {
	b := w.base
	in := w.in
	inLen := len(in)
	en := w.energy
	enLen := len(en)
	ov := w.ov
	a0 := w.prev + w.hs - b
	if a0+ov > inLen {
		return lo // previous grain's continuation not fully buffered
	}
	a := in[a0 : a0+ov : a0+ov]
	var ea float64
	for j := 0; j < ov; j += 2 {
		ea += a[j] * a[j]
	}
	if ea < 1e-12 {
		return lo
	}
	e0 := float64(0)
	if i0 := w.prev + w.hs - b - 1; i0 >= 0 && i0 < enLen {
		e0 = en[i0]
	}
	ea = en[a0+ov-1] - e0 // exact energy of a
	if ea < 1e-12 {
		return lo
	}
	// periodic input makes many positions score ~1; among the near-best
	// prefer the one closest to the nominal center, else stretching
	// systematically shortens and compressing lengthens
	var best, runner float64 = -1, -1
	bestP, runnerP := lo, lo
	for p := lo; p <= hi; p++ {
		bi := p - b
		// valid j are the even indices where in[bi+j] is buffered —
		// compute the range once instead of bounds-checking each
		// multiply; skipped indices contributed nothing to dot anyway
		jlo := 0
		if bi < 0 {
			jlo = -bi
			if jlo&1 != 0 {
				jlo++
			}
		}
		jhi := ov
		if rem := inLen - bi; rem < jhi {
			jhi = rem
		}
		var dot float64
		if jlo < jhi {
			n := jhi - jlo
			seg := in[bi+jlo : bi+jhi]
			as := a[jlo : jlo+n]
			for j := 0; j < n; j += 2 {
				dot += as[j] * seg[j]
			}
		}
		var eb float64
		if bi-1 >= 0 && bi-1 < enLen && bi+ov-1 < enLen {
			eb = en[bi+ov-1] - en[bi-1]
		}
		if eb < 1e-12 {
			eb = 1e-12
		}
		score := dot / math.Sqrt(ea*eb)
		if score > best {
			runner, runnerP = best, bestP
			best, bestP = score, p
		} else if score > runner {
			runner, runnerP = score, p
		}
	}
	if best-runner < 0.02 && math.Abs(float64(runnerP)-c) < math.Abs(float64(bestP)-c) {
		return runnerP
	}
	return bestP
}

// place overlap-adds the grain at absolute position p onto out and ensures
// out holds the full frame (zero padding beyond the input).
func (w *WSOLA) place(p int) {
	for len(w.out) < w.frame {
		w.out = append(w.out, 0)
	}
	b := w.base
	in := w.in
	// the grain's read range [p, p+frame) overlaps the buffered input
	// [b, b+len(in)) only for j in [jlo, jhi); outside it in[p+j-b] was
	// skipped anyway
	jlo := b - p
	if jlo < 0 {
		jlo = 0
	}
	jhi := b + len(in) - p
	if jhi > w.frame {
		jhi = w.frame
	}
	if jlo >= jhi {
		return
	}
	n := jhi - jlo
	i0 := p + jlo - b
	src := in[i0 : i0+n]
	dst := w.out[jlo:jhi]
	win := w.win[jlo:jhi]
	for k := 0; k < n; k++ {
		dst[k] += win[k] * src[k]
	}
}
